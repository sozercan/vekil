# API Reference

Concise endpoint map for Vekil's public API surface. Provider routing is always by public `model` ID; target selection and model/deployment rewriting are internal. Legacy raw OpenAI Chat routes may preserve a nonempty provider-supplied `model`, while explicit routes normalize it to the route public ID. See [Provider Routing](provider-routing.md) for model routes, ownership rules, native endpoint allowlists, budgets, and failover safety, and [Semantic Policy Routing](policy-routing.md) for the narrower schema-v2 policy contract.

For an explicit `model_routes` entry, Vekil returns one proxy-owned `X-Vekil-Request-ID` for the logical HTTP operation. Every target attempt remains inside the same requested public route; `priority_failover` can switch only between ordered, semantically equivalent targets before delivery/progress/commitment becomes ambiguous and never selects another public model route. A schema-v2 policy first seals one terminal route, then uses the same executor only inside that route. Legacy provider-only routes retain their existing retry and fallback behavior.

## `POST /v1/messages` (Anthropic)

Schema-v2 policy public IDs use the same bounded canonical Chat policy planner on this endpoint. The translated request must remain text/function-tool compatible; public responses and errors retain the policy ID.

Anthropic Messages compatibility for the supported content and tool subset. Except for direct `anthropic-compatible` forwarding, requests are translated to canonical OpenAI Chat Completions, resolved through the route that owns the selected public model, executed by Vekil's shared Chat layer, and translated back to Anthropic. Native `/chat/completions` is preferred when allowed; otherwise a native `/responses` model is served through Chat-over-Responses. For `anthropic-compatible` providers, Vekil forwards Messages requests directly to the configured `messages_path`. An explicit route can switch only among its ordered equivalent targets and only before an Anthropic protocol preamble or semantic block is committed.

The native-Chat path supports the existing text/image/tool-use subset, system messages, tool choice, stop sequences, extended thinking via `thinking.type: "enabled"`, and streaming Anthropic SSE translation. Extended thinking preserves `max_tokens` as the hard per-response limit. An interleaved thinking budget may exceed that limit only with the exact interleaved-thinking beta, active tools, and omitted or `auto` tool choice; forced `any` or named-tool choices are rejected with thinking. When the selected model is Responses-native, the translated request must also fit the strict [Responses-backed Chat field subset](#responses-backed-chat-request-subset). In particular, non-empty Anthropic `stop_sequences` are rejected because the initial adapter does not emulate stop sequences locally.

Model normalization strips dated suffixes such as `claude-sonnet-4-20250514` and maps hyphenated version numbers to dotted form, for example `claude-sonnet-4-5` to `claude-sonnet-4.5`.

## `POST /v1/messages/count_tokens` (Anthropic)

Schema-v2 policy public IDs translate count-token input to the canonical Chat probe, select the policy terminal under the active mode, suppress normal traffic statistics for the probe, and return the selected upstream's reported prompt-token usage.

Anthropic count-tokens compatibility for clients such as Claude Code. For Chat-compatible providers, Vekil translates the Messages request to Chat Completions, sends a small non-streaming probe through the same Chat execution layer, and returns reported `usage.prompt_tokens` as Anthropic `input_tokens`. A native Chat model uses a one-output-token probe. Responses-native models require `max_output_tokens: 16`, so Vekil uses that upstream minimum, omits unsupported sampling controls, consumes usage only, and does not publish any tool replay state from the discarded probe completion. For `anthropic-compatible` providers, Vekil directly forwards count-tokens requests to `{messages_path}/count_tokens`.

This endpoint is a compatibility probe rather than a local tokenizer. It can make a small upstream inference request, counts follow the owning provider's reported prompt-token usage, and missing usage is an error rather than a local estimate.

## `GET /v1/models`

The proxy builds a merged logical catalog across legacy provider models and explicit routes. It preserves OpenAI-style `data` and also adds a Codex-compatible top-level `models` array.

```bash
curl http://localhost:1337/v1/models
```

Each public model ID has one logical owner. Dynamic providers can be narrowed with `include_models` or `exclude_models`; static providers such as Azure OpenAI can expose a deployment under a different public ID while the proxy rewrites the upstream `model` field. A public explicit route appears exactly once in both catalog views, in route configuration order, with `owned_by` set to the route ID. Its physical target IDs and upstream deployment names are not emitted as models.

A schema-v2 policy profile also appears exactly once, with `id` equal to its `public_id`, `owned_by: "vekil-policy"`, and `supported_endpoints: ["/chat/completions"]`. Its published tool/context contract is the conservative intersection of the two terminal routes, `vision` is always false in v1, and client-selectable `reasoning_effort` metadata is always omitted. An optional private the `lightweight` and `powerful` tier objects selects the effective effort after classification and is not published. Internal destination and classifier routes do not appear in either catalog view and their operational IDs are not client aliases.

For an explicit route, catalog identity is configuration-owned: temporary target failure does not remove or rewrite the route entry. Endpoint metadata remains **native upstream capability metadata**. A Responses-native model continues to report only `/responses` in `supported_endpoints` even when Vekil serves `/v1/chat/completions`, Anthropic Messages, and Gemini compatibility through Chat-over-Responses.

The exact catalog still depends on configured routes/providers and dynamic discovery. Query `/v1/models` in your deployment instead of hard-coding one global model list.

## Gemini Compatibility Endpoints

Schema-v2 policy public IDs are unsupported on Gemini routes in v1 and fail locally. Use a direct public model/route ID for `generateContent`, `streamGenerateContent`, or `countTokens`.

The proxy accepts all three Gemini route prefixes: `/v1beta/models/{model}:...`, `/v1/models/{model}:...`, and `/models/{model}:...`.

Supported operations:

- `generateContent`
- `streamGenerateContent`
- `countTokens`

Gemini compatibility is a translation layer over canonical Chat Completions. For explicit routes, target selection and any safe failover stay inside the route that owns the requested public model. The translated request may use native `/chat/completions` or native `/responses`, with native Chat preferred when both are available. See [Gemini Compatibility](gemini.md) for the Responses-native restrictions, route commitment rules, ignored fields, explicit `501 UNIMPLEMENTED` cases, validation behavior, and streaming details.

## `POST /v1/chat/completions` (OpenAI)

When `model` resolves to a schema-v2 policy profile, Vekil applies the policy before normal route execution. Policy requests must be text-only and may use only standard function tools. Both policy destinations must support canonical Chat through native `/chat/completions` or bounded Chat-over-Responses execution. Unsupported content or fields fail locally instead of falling through to a direct route.

The policy follows its YAML `off`/`observe`/`enforce` mode by default. An explicit `--policy-routing` / `POLICY_ROUTING_MODE` value applies a process-wide ceiling. `off` and `observe` execute `baseline_tier`; observe classification is asynchronous and cannot change the response path. `enforce` selects one terminal tier synchronously, then seals that route before the first terminal send. Classifier admission/infrastructure failure uses `classifier_unavailable_tier`; abstention or invalid structured output uses `classifier_uncertain_tier`.

Physical failover remains inside the selected terminal route. A failed lightweight route never invokes powerful, a failed powerful route never downgrades, and no quality retry occurs after semantic output. Successful JSON/SSE, safe model headers, errors, and client-facing metrics retain the policy profile public ID; terminal provider/route/target/deployment IDs are not exposed. Direct public route behavior is unchanged.

Vekil resolves the public model once and chooses its native backend:

1. use native `/chat/completions` when the provider and model allow it;
2. otherwise use native `/responses` and convert the request and result through the Chat-over-Responses adapter;
3. reject the request locally when neither native endpoint is allowed.

Provider policy plus the selected model's or explicit route's native endpoint allowlist control this choice. A `/responses`-only model is served through the adapter rather than probed at an unsupported Chat path; Vekil fails locally with `400` only when neither native endpoint is allowed. Explicit route targets remain ordered and bounded by the route's target-attempt and upstream-send budgets.

A native Chat request remains near-zero-copy. Successful non-streaming responses are conservatively normalized to strict OpenAI Chat Completions shape when upstreams omit required compatibility fields: missing `object` becomes `"chat.completion"`, missing `created`/`id`/`usage` are filled, requested `model` is used when absent, and choice `message`/`index`/`finish_reason` gaps are repaired while vendor-specific metadata is preserved. Streaming chunks are similarly repaired only for missing envelope fields such as `object: "chat.completion.chunk"`, `id`, `created`, `model`, `choices[].index`, and `choices[].delta`. Legacy raw Chat routes preserve a nonempty upstream `model`; explicit routes replace physical model/deployment values with the route public ID in supported JSON and SSE responses. When tools are present, Vekil injects `parallel_tool_calls: true`, may force upstream streaming for reliable parallel calls, and aggregates back to non-streaming JSON when the client did not request a stream.

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
- For a policy public ID, incoming Chat `reasoning_effort`, Anthropic `output_config.effort`, or Responses `reasoning.effort` is accepted only when the profile configures tier `reasoning_effort`; the selected tier value then replaces it before terminal execution. An unmapped profile rejects incoming effort as unsupported and injects nothing when effort is omitted. Client effort cannot force a tier or override a mapped profile. Direct non-policy routes continue to preserve supported client effort.
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

Schema-v2 policy public IDs are accepted by bounded stateless `POST /v1/responses` compatibility. It requires `store: false` or omitted, rejects `previous_response_id`, accepts text, bounded `text.format` (`text`, `json_object`, or `json_schema`) structured output, and function/namespace-child tools, converts the request to canonical Chat for policy planning, and converts the aggregated Chat result back to Responses JSON/SSE. Completed terminal output must honor required or forced `tool_choice` constraints. Hosted/custom tools, images, the websocket bridge, compact, memory summarization, and native Responses terminal selection remain unsupported for policy IDs. Existing direct Responses behavior is unchanged.

Supported OpenAI/Codex-style routes:

- `POST /v1/responses` — near zero-copy Responses passthrough with proxy-owned compaction item expansion.
- `GET /v1/responses` — optional Codex-style websocket bridge when `--responses-ws-enabled` or `RESPONSES_WS_ENABLED=true` is set.
- `POST /v1/responses/compact` — compatibility shim that rewrites compact requests into upstream `/responses` calls and returns a proxy-owned opaque compaction item.
- `POST /v1/memories/trace_summarize` — compatibility shim for trace and memory summaries.

Responses requests are routed by public `model` ID. A version-2 `priority_failover` route may switch among equivalent targets in that same public route only while replay is definitely safe and no provider-bound state or downstream commitment prevents it. Legacy unsupported-model compatibility fallback remains distinct, stays within the selected provider/target, and never becomes configured cross-route or cross-provider fallback. For Responses-only Azure deployments, `POST /v1/responses` remains the canonical native inference path even when Chat-compatible surfaces are available through translation.

See [OpenAI Responses Compatibility](responses.md) for compaction, oversized replay, transient streaming failure, websocket semantics, and the separation between native Responses and Chat-over-Responses compatibility. See [Responses WebSocket Bridge](responses-websocket.md) for websocket tuning flags.

## `GET /healthz`

Returns `{"status":"ok"}` as soon as the HTTP listener is serving. This endpoint reports process liveness and does not wait for provider authentication.

## `GET /readyz`

Validates compiled configuration, required startup authentication/catalog initialization, admission state, policy classifier preflight where required, and shutdown state. On success it returns `{"status":"ready"}`. On failure it returns `503` with `{"status":"not_ready","error":"..."}`. Transient failure of one terminal route target does not by itself make the whole process unready. An effective policy `enforce` profile must pass live classifier preflight before startup/readiness completes; an effective `observe` profile that fails preflight is kept off and reports a readiness/configuration diagnostic.

## Traffic Dashboard Endpoints

- `GET /dashboard` — live, browser-based traffic dashboard served from the proxy (available wherever the proxy runs).
- `GET /stats.json` — in-memory traffic snapshot (client-request totals, latency percentiles, per-second series, by-model/provider/agent/route/target breakdowns, physical upstream attempts, target switches, state-binding counters, legacy upstream retries, policy eligibility/admission/decision/classifier metrics, and a recent-requests log) polled by the dashboard.
- `POST /dashboard/insight` — optional AI-generated traffic summary. Active only when `insight_model` is set in providers config; single-flight with a cooldown; fails open. The insight model may be native Chat or native Responses as long as Vekil can serve it through Chat compatibility.

These endpoints are unauthenticated like `/healthz` and are excluded from their own stats. See [Traffic Dashboard](dashboard.md) for the payload shape, agent classification, AI insights, and access/security notes.
