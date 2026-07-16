# API Reference

Concise endpoint map for Vekil's public API surface. Provider routing is always by public `model` ID; provider-specific upstream model/deployment names stay internal to the proxy. See [Provider Routing](provider-routing.md) for ownership rules and endpoint allowlists.

## `POST /v1/messages` (Anthropic)

Anthropic Messages compatibility for the supported content and tool subset. Requests are normally translated to canonical OpenAI Chat Completions, sent through Vekil's Chat execution layer, and translated back to Anthropic. The selected model may be backed natively by `/chat/completions` or by `/responses`; native Chat is preferred when both are available. For `anthropic-compatible` providers, the proxy instead forwards Messages requests directly to the configured `messages_path`.

The native-Chat path supports the existing text/image/tool-use subset, system messages, tool choice, stop sequences, extended thinking via `thinking.type: "enabled"`, and streaming Anthropic SSE translation. Extended thinking preserves `max_tokens` as the hard per-response limit. An interleaved thinking budget may exceed that limit only with the exact interleaved-thinking beta, active tools, and omitted or `auto` tool choice; forced `any` or named-tool choices are rejected with thinking. When the selected model is Responses-native, the translated request must also fit the strict [Responses-backed Chat field subset](#responses-backed-chat-request-subset). In particular, non-empty Anthropic `stop_sequences` are rejected because the initial adapter does not emulate stop sequences locally.

Model normalization strips dated suffixes such as `claude-sonnet-4-20250514` and maps hyphenated version numbers to dotted form, for example `claude-sonnet-4-5` to `claude-sonnet-4.5`.

## `POST /v1/messages/count_tokens` (Anthropic)

Anthropic count-tokens compatibility for clients such as Claude Code. For Chat-compatible providers, Vekil translates the Messages request to Chat Completions, sends a small non-streaming probe through the same Chat execution layer, and returns reported `usage.prompt_tokens` as Anthropic `input_tokens`. A native Chat model uses a one-output-token probe. Responses-native models require `max_output_tokens: 16`, so Vekil uses that upstream minimum, omits unsupported sampling controls, consumes usage only, and does not publish any tool replay state from the discarded probe completion. For `anthropic-compatible` providers, Vekil directly forwards count-tokens requests to `{messages_path}/count_tokens`.

This endpoint is a compatibility probe rather than a local tokenizer. It can make a small upstream inference request, counts follow the owning provider's reported prompt-token usage, and missing usage is an error rather than a local estimate.

## `GET /v1/models`

The proxy builds a merged catalog across active providers. It preserves OpenAI-style `data` and also adds a Codex-compatible top-level `models` array.

```bash
curl http://localhost:1337/v1/models
```

When multiple providers are configured, each public model ID must have one owner. Dynamic providers can be narrowed with `include_models` or `exclude_models`; static providers such as Azure OpenAI can expose a deployment under a different public ID while the proxy rewrites the upstream `model` field.

Endpoint metadata remains **native upstream capability metadata**. A Responses-native model continues to report `/responses` in `supported_endpoints` even though Vekil can serve `/v1/chat/completions`, Anthropic Messages, and Gemini compatibility through its Chat-over-Responses adapter. Vekil does not add emulated Chat routes to `models[].endpoints` or the rendered catalog.

The exact catalog depends on configured providers and current upstream availability. Query `/v1/models` in your deployment instead of hard-coding one global model list.

## Gemini Compatibility Endpoints

The proxy accepts all three Gemini route prefixes: `/v1beta/models/{model}:...`, `/v1/models/{model}:...`, and `/models/{model}:...`.

Supported operations:

- `generateContent`
- `streamGenerateContent`
- `countTokens`

Gemini compatibility is a translation layer over canonical Chat Completions. The translated request can be executed through native Chat or native Responses, with the route-specific restrictions described in [Gemini Compatibility](gemini.md).

## `POST /v1/chat/completions` (OpenAI)

Vekil resolves the public model once and chooses its native backend:

1. use native `/chat/completions` when the provider and model allow it;
2. otherwise use native `/responses` and convert the request and result through the Chat-over-Responses adapter;
3. reject the request locally when neither native endpoint is allowed.

A native Chat request remains near-zero-copy. Successful non-streaming responses are conservatively normalized when upstreams omit required compatibility fields, and streamed chunks are similarly repaired only where required. When tools are present, Vekil injects `parallel_tool_calls: true`, may force upstream streaming for reliable parallel calls, and aggregates back to non-streaming JSON when the client did not request a stream.

A Responses-backed request is not passthrough. Vekil validates the supported Chat request shape, converts it to Responses input, converts the result to one canonical Chat choice, and rejects unsupported fields rather than silently dropping them. Direct `/v1/responses` behavior is separate; the adapter does not invoke the public Responses handler, compaction shims, or websocket bridge.

Streamed traffic records terminal usage when the selected backend provides it. On native Chat, Vekil may inject `stream_options.include_usage` when the client omitted it; that proxy-injected usage chunk is consumed internally and not forwarded to a client that did not request it. Responses streams use their terminal usage data directly. A client-supplied `stream_options.include_usage` is honored.

### Responses-backed Chat request subset

The following matrix applies only when the selected model is executed through native `/responses`. Native Chat providers retain their existing near-zero-copy behavior, including vendor extensions.

| Behavior | Chat fields |
|---|---|
| **Mapped** | `model`, `messages`, `stream`, `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, function `tools`, `tool_choice`, `parallel_tool_calls`, `response_format`, `reasoning_effort`, `verbosity`, `metadata`, `store`, `user`, `prompt_cache_key`, `safety_identifier` |
| **Local only** | `stream_options.include_usage` |
| **Conditionally accepted** | `stop` only when omitted, `null`, `""`, or `[]`; `n` only when omitted, `null`, or `1` |
| **Rejected** | `service_tier`; non-empty `stop`; `n` other than `1`; penalties including `frequency_penalty` and `presence_penalty`; `seed`; `logit_bias`; `logprobs` and `top_logprobs`; `audio`; `modalities`; `prediction`; legacy `functions`; non-function/hosted/custom tools; `messages[].name`; unknown top-level fields |

Additional rules:

- `model` is required and `messages` must be a non-empty array.
- `max_tokens` and `max_completion_tokens` both map to `max_output_tokens`; if both are present they must be equal.
- Because the native Responses endpoint requires at least 16 output tokens, a client-supplied limit from 0 through 15 is rejected locally on Responses-backed routes rather than silently clamped beyond the Chat upper bound. Native Chat routes retain non-streaming `max_tokens: 0` prewarm requests when thinking is disabled; Vekil returns empty content with `stop_reason: "max_tokens"`, and rejects zero-token requests that enable streaming or thinking.
- `response_format` accepts `text`, `json_object`, and `json_schema`.
- `tool_choice` accepts `none`, `auto`, `required`, or a declared named function.
- Message content supports text and ordered content parts. HTTP(S) images and base64 image data URLs are accepted only in user messages.
- Only function tools are supported. Hosted Responses tools such as web search, computer use, code execution, image generation, and other non-function tool types are not available through Chat compatibility.
- Vekil does not implement local stop matching in this release. Non-empty `stop` fails with `400 invalid_request_error`, `param: "stop"`.
- Unknown top-level fields and explicitly unsupported subset fields fail with `400 invalid_request_error` and the relevant field in `error.param`.
- When Chat `store` is omitted or `null`, Vekil sends `store: false` so routing through Responses preserves Chat's non-storage default. An explicit `store: true` is preserved.
- Vekil internally requests `reasoning.encrypted_content` so storage-disabled reasoning/tool turns remain statelessly replayable; this internal include is not a public Chat field.
- A provider policy may remove an otherwise mapped sampling field when that native Responses backend does not accept it; for example, the OpenAI Codex provider removes `temperature` and `top_p`.

### Responses-backed tool continuation

Function calls returned through the adapter use opaque IDs shaped as:

```text
call_vekil_<22-character-base64url>
```

Clients must return these IDs unchanged. They are keys into process-local replay state that retains the exact hidden Responses output needed for tool continuation; upstream `call_id` values are never exposed as the authorization key.

For a parallel call group, the assistant `tool_calls` projection must remain complete and in its original order. A complete set of tool-result messages may arrive in any order. If only a non-empty subset of results is available, Vekil replays only the matching prior function calls plus their outputs; this partial projection is required because the verified Responses backend rejected a complete parallel call group paired with only partial outputs. The missing calls may consequently be reissued by the model.

Replay state has an absolute one-hour lifetime and is bounded to 2,048 groups, 2 MiB per group, 64 MiB total, 256 output items per group, and 128 calls per group, with LRU eviction under group/byte pressure. It is memory-only and is lost on restart. Expired, evicted, forged, cross-route, or post-restart `call_vekil_...` IDs fail locally with:

```text
HTTP 400
error.type = invalid_request_error
error.code = responses_replay_state_missing
error.param = messages
error.message = Responses-backed tool state is no longer available; restart the assistant tool-call turn.
```

The client should restart that assistant tool-call turn rather than attempting to reconstruct hidden replay state.

## Responses Compatibility Endpoints

Supported OpenAI/Codex-style routes:

- `POST /v1/responses` — near zero-copy Responses passthrough with proxy-owned compaction item expansion.
- `GET /v1/responses` — optional Codex-style websocket bridge when `--responses-ws-enabled` or `RESPONSES_WS_ENABLED=true` is set.
- `POST /v1/responses/compact` — compatibility shim that rewrites compact requests into upstream `/responses` calls and returns a proxy-owned opaque compaction item.
- `POST /v1/memories/trace_summarize` — compatibility shim for trace and memory summaries.

Responses requests are routed by public `model` ID. Unsupported-model fallbacks stay within the selected provider; the proxy does not silently switch providers. For responses-only Azure deployments, `POST /v1/responses` remains the canonical native inference path even when Chat-compatible surfaces are also available through translation.

See [OpenAI Responses Compatibility](responses.md) for compaction, oversized replay, transient streaming failure, websocket semantics, and the separation between native Responses and Chat-over-Responses compatibility. See [Responses WebSocket Bridge](responses-websocket.md) for websocket tuning flags.

## `GET /healthz`

Returns `{"status":"ok"}` as soon as the HTTP listener is serving. This endpoint reports process liveness and does not wait for provider authentication.

## `GET /readyz`

Validates that the proxy can authenticate to and successfully probe the configured upstream providers. On success it returns `{"status":"ready"}`. On failure it returns `503` with `{"status":"not_ready","error":"..."}`.

## Traffic Dashboard Endpoints

- `GET /dashboard` — live, browser-based traffic dashboard served from the proxy (available wherever the proxy runs).
- `GET /stats.json` — in-memory traffic snapshot (totals, latency percentiles, per-second series, by-model/provider/agent breakdowns, upstream retries, and a recent-requests log) polled by the dashboard.
- `POST /dashboard/insight` — optional AI-generated traffic summary. Active only when `insight_model` is set in providers config; single-flight with a cooldown; fails open. The insight model may be native Chat or native Responses as long as Vekil can serve it through Chat compatibility.

These endpoints are unauthenticated like `/healthz` and are excluded from their own stats. See [Traffic Dashboard](dashboard.md) for the payload shape, agent classification, AI insights, and access/security notes.
