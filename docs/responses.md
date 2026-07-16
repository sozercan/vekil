# OpenAI Responses Compatibility

Detailed behavior for Vekil-owned Responses compatibility features. For the generic endpoint list, see [API Reference](api.md). For websocket tuning flags, see [Responses WebSocket Bridge](responses-websocket.md).

## `POST /v1/responses` and `GET /v1/responses` (OpenAI)

`POST /v1/responses` is a near zero-copy passthrough for the OpenAI Responses API. Proxy-owned synthetic compaction items are expanded back into normal context before forwarding so resumed Codex sessions continue through the standard `/v1/responses` path. When a follow-up carries both a proxy-generated response ID (`resp-vekil-compact-*`) and its proxy-owned checkpoint, Vekil removes that synthetic `previous_response_id` and stale `X-Codex-Turn-State` before forwarding because neither value exists upstream; ordinary upstream response IDs and turn state remain unchanged. For provider routes other than the OpenAI Codex backend, Vekil also removes Codex client-only `internal_chat_message_metadata_passthrough` fields from `input` items before forwarding because Copilot, Azure, and compatible upstreams reject that internal metadata as an unknown Responses parameter.

`POST /v1/responses` requests are accepted up to `64 MiB` so oversized session replays can reach the proxy-owned compaction fallback instead of failing at the default request-body limit. If upstream rejects a replay-like request with `413 Payload Too Large`, the proxy can compact the older prefix of an array `input` and retry the request with one proxy-owned checkpoint plus the most recent tail items. A request is treated as replay-like only when the body contains prior transcript evidence such as assistant/tool output items or a proxy-owned compaction checkpoint; `previous_response_id` and Codex turn/window headers alone do not trigger summarization. The fallback starts by preserving the configured tail, aligns the tail boundary so tool/function call-output pairs are not split across the checkpoint, and under `413` pressure dynamically halves the preserved tail (`12 -> 6 -> 3 -> 1`, or `len(input)-1` first for shorter replays) and retries until the compacted replay fits or only the latest item remains. The compaction calls share the same attempt budget and chunking controls as `/v1/responses/compact` (`--compact-upstream-chunk-bytes` and `--compact-upstream-max-attempts`). When a provider/model returns `413` for a compaction body, the proxy remembers the smaller target in memory for 30 minutes and starts later compaction fallbacks for that provider/model at the learned target, while the normal no-413 `/v1/responses` fast path remains unchanged. If the internal compaction result is incomplete, lacks nonempty message output text, or exceeds the summary-response limit, Vekil returns the original `413` instead of retrying with an empty or partial checkpoint.

Like chat completions, Responses requests are routed by the public `model` ID. Legacy unsupported-model fallback remains on the selected provider. A version-2 `priority_failover` route is different: it may try the next equivalent target in that same public route, but it never falls back to another public model route.

For responses-only Azure deployments such as the `gpt-5.4-pro` example configuration, this is the canonical inference path.

Streaming Responses requests preserve upstream headers. Before flushing `200 OK`, the proxy can translate a transient `response.failed` admission error such as `too_many_requests`, numeric `429`, `model_overloaded`, `bad_gateway`, or `gateway_timeout` into a normal HTTP error. A top-level Responses `error` event is also terminal and is translated before commit; error-event headers such as `retry-after-ms` and request IDs participate in retry metadata and diagnostics. Client-facing `Retry-After` preserves the provider's authoritative delay even when it exceeds Vekil's separate five-minute cap for internal retry sleeps. For upstreams such as Azure Foundry that may emit `response.created` or `response.in_progress` before an admission failure, Vekil can briefly retain those preamble events when the response headers show exhausted quota. An uncoded failure, or one carrying only the generic `server_error` type, is treated as HTTP `429` only when its message indicates rate limiting and the quota telemetry is corroborating; specific non-rate-limit codes remain authoritative. A final terminal event is still recognized when the upstream closes at EOF without the usual blank SSE delimiter. The retained preamble is time- and size-bounded. Once a non-preamble/output event appears, the HTTP stream is committed and later failures remain passthrough SSE, while provider accounting still records their classified failure status. Explicit routes additionally apply their stricter target-switch, public-model normalization, and exact state-binding rules below.

If an upstream rejects a `/responses` request with `400 invalid_request_body` because replayed `encrypted_content` cannot be verified or decrypted, Vekil retries once after removing the opaque encrypted replay items it cannot interpret. The original request remains near-zero-copy when the selected upstream accepts its own encrypted tokens; the retry only applies after that specific upstream rejection. Vekil-owned compaction checkpoints are still expanded to developer context before forwarding.

### Explicit HTTP route failover and commitment

For a schema-version-2 explicit HTTP route operation, one logical Responses operation owns a single total deadline, a target-attempt budget, and a physical upstream-send budget. Every normal target attempt consumes one target and one send. Named same-target recovery, compaction/replay child calls, and compatibility-model calls consume additional sends and stay pinned to the selected target; if a reachable path cannot participate in the shared budget and pinning contract, Vekil rejects it for an explicit route rather than starting unbudgeted work. Redirects and implicit request replay are disabled.

Non-streaming `priority_failover` can move to the next target only after a definitely prewrite transport failure, authoritative `429`, or another adapter-certified pre-execution overload/unavailable rejection. A reset or timeout after request write, generic gateway error, partial `2xx` body, authentication/configuration/client error, total deadline, cleanup uncertainty, or any ambiguous delivery is terminal for route selection.

For streaming `/responses`, explicit failover routes hold only `response.created` and `response.in_progress` as an allowed preamble. The preamble is buffered until semantic/tool output, an authoritative terminal event, the existing `750 ms` precommit timeout, or the existing `64 KiB` bound. An adapter-certified terminal admission failure that proves no semantic/tool execution can discard the held preamble and try the next target. Reaching either bound commits the selected target and fails open to normal streaming. Text, reasoning, tool activity, malformed/unknown events, downstream headers/body bytes, and postcommit `response.failed` or `response.incomplete` permanently prohibit another target; postcommit terminal events are forwarded once.

Every successful same-route response and stream event keeps the requested public model ID. Failed-attempt response headers and request IDs are not merged into a later success. `X-Vekil-Request-ID` identifies the logical client operation; individual upstream request IDs remain attempt-level diagnostics.

### Exact Responses state binding

`previous_response_id`, trusted server-issued `X-Codex-Turn-State`, non-proxy opaque `encrypted_content`, and other adapter-marked opaque response/session artifacts are exact target state. Vekil binds each value to `{route_id, target_id}` immediately before the header, event, frame, or body content exposing it is committed. Hidden state from an abandoned precommit attempt is not stored; state already exposed remains bound even if the selected target later fails.

Explicit Responses requests reject duplicate JSON object keys and differing repeated `X-Codex-Turn-State` values before routing. All state supplied on a follow-up must agree on one owner. Known state routes to that exact target with no failover. Malformed, conflicting, cross-route, mixed known/unknown, or unknown state on an explicit Responses operation fails locally before any upstream call. If the owning target is unavailable, the request fails closed rather than guessing another target. Proxy-owned `resp-vekil-compact-*` checkpoints remain distinguishable from upstream state and continue to use the existing expansion behavior.

The binding index is process-local, capped at 32,768 entries, and uses a 24-hour absolute TTL. Eviction, expiry, restart, or landing the follow-up on another Vekil process makes a prior binding unknown. Stateful multi-target Responses traffic therefore requires one process or sticky ingress. There is no cross-target response-state migration, full-replay portability claim, durable/shared binding store, or circuit-breaker-driven target substitution.

`GET /v1/responses` can upgrade to a websocket bridge for Codex-style clients when `--responses-ws-enabled` or `RESPONSES_WS_ENABLED=true` is set. The proxy:

- accepts `response.create` frames, including websocket-only top-level fields such as `client_metadata` and `initiator`
- uses supported `client_metadata` values to derive request headers, then strips websocket-only metadata before forwarding upstream
- handles warmup and incremental follow-up requests locally
- forwards the active turn to upstream HTTP `/responses`
- relays streamed JSON events back as websocket text frames

Websocket bridge behavior:

- each websocket session is serialized: one active turn is processed at a time
- Vekil does not multiplex turns or implement Copilot-style request superseding
- closing the websocket ends the session; once Vekil observes the disconnect, it stops relaying and closes the upstream response body
- upstream requests use the proxy streaming timeout, while websocket close/read failure, missed pong, and server shutdown still cancel active turn work
- history is stored append-only in memory
- long sessions are auto-compacted into one proxy-owned checkpoint plus a recent tail
- optional turn-state delta replay can be enabled with `--responses-ws-turn-state-delta`
- if upstream rejects delta replay, the proxy automatically falls back to full replay
- if the first meaningful streamed upstream event is a transient `response.failed` admission error or a top-level Responses `error` event, including an uncoded Azure quota failure corroborated by headers, the bridge sends a wrapped websocket error frame instead of relaying the raw terminal event; synthesized `Retry-After` metadata is included when only reset telemetry is available

This websocket bridge is a proxy transport adaptation layered over upstream HTTP `/responses`. It is not the same feature as provider-native websocket or realtime APIs such as Azure `/realtime`.

For a schema-version-2 route, the first provider-backed websocket turn may use safe precommit failover. Once that target is exposed, the session pins to the exact route/target; later turns, delta/full replay, and auto-compaction remain on it, and an unavailable pinned target fails closed. Cross-target session migration is not implemented. Drain sessions before a version-1 or older-binary rollback.

See [responses-websocket.md](responses-websocket.md) for tuning flags.

## `POST /v1/responses/compact`

Compatibility shim for environments expecting `/responses/compact`. The proxy rewrites the request into a normal upstream `/responses` call with a compaction prompt, then returns:

- retained `system`, `developer`, and `user` messages from the original compact request
- one proxy-owned opaque `compaction` item whose `encrypted_content` can later be sent back to `/v1/responses` or `/v1/responses/compact`

Requests to this endpoint are accepted up to `64 MiB` so large session histories can be compacted without tripping the default request-body limit. Internal compaction calls preserve model/provider/routing fields, but remove caller tools and tool selection, streaming/background controls, structured-output constraints (`text.format` and `response_format`), and caller output-token caps so checkpoint generation cannot be diverted into a tool call, schema-only response, or trivially truncated output. A successful upstream summary body is buffered up to `1 MiB`, must be `completed` when a status is present, and must contain nonempty message output text; incomplete, refusal-only, function-call-only, empty, or oversized results fail without emitting a proxy checkpoint.

If the upstream `/responses` call still rejects the compact payload with `413 Payload Too Large`, the proxy retries by compacting the input in chunks, including splitting oversized individual input items or string inputs into synthetic historical-context chunks before merging the partial summaries into one final checkpoint. For an explicit route, chunk sends are serialized even when the general chunk-concurrency setting is higher, and both the compact attempt cap and the route's `max_upstream_sends` apply. The chunk target body size starts at `4 MiB` (sized below the empirically observed Copilot `/responses` cap of `5 MiB`; configurable via `--compact-upstream-chunk-bytes` / `COMPACT_UPSTREAM_CHUNK_BYTES`) and is halved on each recursive `413` (and eagerly halved when a sub-target body is itself rejected) down to a `64 KiB` floor; once the floor is reached the original `413` is returned to the caller. Total logical compaction calls across the chunk fanout are capped by `--compact-upstream-max-attempts` / `COMPACT_UPSTREAM_MAX_ATTEMPTS` (default `48`, sized to the 64 MiB inbound ceiling with one round of recursive halving) to bound per-request cost; each logical call may add one extra HTTP POST when the configured model is unsupported (model-fallback) and is also subject to the shared transport-retry policy on transient upstream failures, so this cap is a fanout safety net rather than a precise HTTP-POST limit. Once the proxy has resolved a fallback model on the first chunk, subsequent chunks reuse it directly and skip the model-fallback probe. Budget exhaustion or an invalid chunk/merge summary surfaces the original `413`. If the inbound request has `input:[]` and the upstream still rejects it, the original `413` is returned rather than a fabricated empty checkpoint.

If the requested model does not support the upstream Responses API, the proxy retries against a compatible fallback model discovered from `/models`. That compatibility fallback stays within the selected provider/target and is distinct from configured route failover. On an explicit route it consumes the logical operation's upstream-send budget and cannot open an unbounded retry/failover tree.

## `POST /v1/memories/trace_summarize`

Compatibility shim that summarizes one or more traces into `{trace_summary, memory_summary}` objects using the upstream `/responses` endpoint plus a JSON-only summarization prompt.

Requests to this endpoint are accepted up to `64 MiB` so larger trace bundles can be summarized in a single call. Successful upstream summary responses use the same `1 MiB` buffer limit and completed/nonempty-output validation as compaction responses. Route-aware internal calls remain bounded by the parent operation/send budget and pinned to the selected target; they do not independently fan out across routes.
