# API Reference

Concise endpoint map for Vekil's public API surface. Provider routing is always by public `model` ID; target selection and model/deployment rewriting are internal. Legacy raw OpenAI Chat routes may preserve a nonempty provider-supplied `model`, while explicit routes normalize it to the route public ID. See [Provider Routing](provider-routing.md) for version-2 model routes, ownership rules, endpoint allowlists, budgets, and failover safety.

For an explicit `model_routes` entry, Vekil returns one proxy-owned `X-Vekil-Request-ID` for the logical HTTP operation. Every target attempt remains inside the same requested public route; `priority_failover` can switch only between ordered, semantically equivalent targets before delivery/progress/commitment becomes ambiguous and never selects another public model route. Legacy provider-only routes retain their existing retry and fallback behavior.

## `POST /v1/messages` (Anthropic)

Anthropic Messages compatibility for the supported content and tool subset. Requests are usually translated to OpenAI Chat Completions, routed through the route that owns the selected public model, and translated back to Anthropic. For `anthropic-compatible` providers, the proxy directly forwards Messages requests to the configured `messages_path`. An explicit route must advertise the corresponding canonical operation, and any allowed target switch must happen before an Anthropic protocol preamble or semantic block is committed.

Supported features include text/image/tool-use content blocks, system messages, tool choice, stop sequences, extended thinking via `thinking.type: "enabled"`, and streaming Anthropic SSE event translation.

Model normalization strips dated suffixes such as `claude-sonnet-4-20250514` and maps hyphenated version numbers to dotted form, for example `claude-sonnet-4-5` to `claude-sonnet-4.5`.

## `POST /v1/messages/count_tokens` (Anthropic)

Anthropic count-tokens compatibility for clients such as Claude Code. For OpenAI-compatible upstreams, Vekil translates the Messages request to Chat Completions, sends a minimal one-token probe, and returns `usage.prompt_tokens` as Anthropic `input_tokens`. For `anthropic-compatible` providers, the proxy directly forwards count-tokens requests to `{messages_path}/count_tokens`.

The endpoint is a compatibility probe rather than a native tokenizer. It may make a small upstream inference request, and counts follow the owning provider's reported prompt-token usage.

## `GET /v1/models`

The proxy builds a merged logical catalog across legacy provider models and explicit routes. It preserves OpenAI-style `data` and also adds a Codex-compatible top-level `models` array.

```bash
curl http://localhost:1337/v1/models
```

Each public model ID has one logical owner. Dynamic providers can be narrowed with `include_models` or `exclude_models`; static providers such as Azure OpenAI can expose a deployment under a different public ID while the proxy rewrites the upstream `model` field. A version-2 explicit route appears exactly once in both catalog views, in route configuration order, with `owned_by` set to the route ID. Its physical target IDs and upstream deployment names are not emitted as models.

Catalog identity is configuration-owned: temporary target failure does not remove or rewrite a route entry. The exact catalog still depends on configured routes/providers and dynamic discovery, so query `/v1/models` in your deployment instead of hard-coding one global model list.

## Gemini Compatibility Endpoints

The proxy accepts all three Gemini route prefixes: `/v1beta/models/{model}:...`, `/v1/models/{model}:...`, and `/models/{model}:...`.

Supported operations:

- `generateContent`
- `streamGenerateContent`
- `countTokens`

Gemini compatibility is a translation layer over OpenAI Chat Completions. For explicit routes, route selection and any safe failover happen around that canonical operation while Gemini routing and catalog identity remain the requested public ID. See [Gemini Compatibility](gemini.md) for supported fields, route commitment rules, ignored fields, explicit `501 UNIMPLEMENTED` cases, validation behavior, and streaming details.

## `POST /v1/chat/completions` (OpenAI)

Near zero-copy passthrough for requests without tools. Successful non-streaming responses are conservatively normalized to strict OpenAI Chat Completions shape when upstreams omit required compatibility fields: missing `object` becomes `"chat.completion"`, missing `created`/`id`/`usage` are filled, requested `model` is used when absent, and choice `message`/`index`/`finish_reason` gaps are repaired while vendor-specific metadata is preserved. Streaming chat chunks are similarly normalized only for missing chunk envelope fields such as `object: "chat.completion.chunk"`, `id`, `created`, `model`, `choices[].index`, and `choices[].delta`. Legacy raw Chat routes preserve a nonempty upstream `model` for compatibility. Explicit routes instead replace nonempty physical model/deployment values with the route public ID in supported JSON and SSE responses, so failover topology is not exposed.

When tools are present, the proxy injects `parallel_tool_calls: true`, forces upstream streaming for reliable parallel tool calls, and aggregates the result back to non-streaming JSON.

The proxy enforces the selected model route's configured endpoint allowlist before forwarding. If a model is `/responses`-only, `POST /v1/chat/completions` fails fast with `400` instead of probing an unsupported upstream route. Explicit route targets are attempted only in configured order and only under the route's operation/send budgets.

For client-requested streams that omit `stream_options`, the proxy asks the upstream for a final usage chunk (`stream_options.include_usage`) so streamed traffic still records token totals in the dashboard. That injected usage-only chunk is consumed internally and **not** forwarded to the client, so clients do not see an extra `choices: []` terminal chunk. If the client supplied its own `stream_options`, its choice is preserved untouched.

## Responses Compatibility Endpoints

Supported OpenAI/Codex-style routes:

- `POST /v1/responses` — near zero-copy Responses passthrough with proxy-owned compaction item expansion.
- `GET /v1/responses` — optional Codex-style websocket bridge when `--responses-ws-enabled` or `RESPONSES_WS_ENABLED=true` is set.
- `POST /v1/responses/compact` — compatibility shim that rewrites compact requests into upstream `/responses` calls and returns a proxy-owned opaque compaction item.
- `POST /v1/memories/trace_summarize` — compatibility shim for trace and memory summaries.

Responses requests are routed by public `model` ID. A version-2 `priority_failover` route may switch among its equivalent configured targets only while replay is definitely safe and no provider-bound state or downstream commitment prevents it. Existing unsupported-model compatibility fallback remains distinct, stays on the selected target/provider, and never becomes configured cross-route model fallback. For responses-only Azure deployments, `POST /v1/responses` is the canonical inference path.

See [OpenAI Responses Compatibility](responses.md) for compaction, oversized replay, transient streaming failure, websocket semantics, and compatibility-shim details. See [Responses WebSocket Bridge](responses-websocket.md) for websocket tuning flags.

## `GET /healthz`

Returns `{"status":"ok"}` as soon as the HTTP listener is serving. This endpoint reports process liveness and does not wait for provider authentication.

## `GET /readyz`

Validates compiled configuration, required startup authentication/catalog initialization, admission state, and shutdown state. On success it returns `{"status":"ready"}`. On failure it returns `503` with `{"status":"not_ready","error":"..."}`. Transient failure of one route target does not by itself make the whole process unready.

## Traffic Dashboard Endpoints

- `GET /dashboard` — live, browser-based traffic dashboard served from the proxy (available wherever the proxy runs).
- `GET /stats.json` — in-memory traffic snapshot (client-request totals, latency percentiles, per-second series, by-model/provider/agent/route/target breakdowns, physical upstream attempts, target switches, state-binding counters, legacy upstream retries, and a recent-requests log) polled by the dashboard.
- `POST /dashboard/insight` — optional AI-generated traffic summary. Active only when `insight_model` is set in providers config; single-flight with a cooldown; fails open.

These endpoints are unauthenticated like `/healthz` and are excluded from their own stats. See [Traffic Dashboard](dashboard.md) for the payload shape, agent classification, AI insights, and access/security notes.
