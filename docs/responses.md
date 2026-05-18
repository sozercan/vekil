# OpenAI Responses Compatibility

Detailed behavior for Vekil-owned Responses compatibility features. For the generic endpoint list, see [API Reference](api.md). For websocket tuning flags, see [Responses WebSocket Bridge](responses-websocket.md).

## `POST /v1/responses` and `GET /v1/responses` (OpenAI)

`POST /v1/responses` is a near zero-copy passthrough for the OpenAI Responses API. Proxy-owned synthetic compaction items are expanded back into normal context before forwarding so resumed Codex sessions continue through the standard `/v1/responses` path.

`POST /v1/responses` requests are accepted up to `64 MiB` so oversized session replays can reach the proxy-owned compaction fallback instead of failing at the default request-body limit. If upstream rejects a replay-like request with `413 Payload Too Large`, the proxy can compact the older prefix of an array `input` and retry the request with one proxy-owned checkpoint plus the most recent tail items. A request is treated as replay-like only when the body contains prior transcript evidence such as assistant/tool output items or a proxy-owned compaction checkpoint; `previous_response_id` and Codex turn/window headers alone do not trigger summarization. The fallback starts by preserving the configured tail, aligns the tail boundary so tool/function call-output pairs are not split across the checkpoint, and under `413` pressure dynamically halves the preserved tail (`12 -> 6 -> 3 -> 1`, or `len(input)-1` first for shorter replays) and retries until the compacted replay fits or only the latest item remains. The compaction calls share the same attempt budget and chunking controls as `/v1/responses/compact` (`--compact-upstream-chunk-bytes` and `--compact-upstream-max-attempts`). When a provider/model returns `413` for a compaction body, the proxy remembers the smaller target in memory for 30 minutes and starts later compaction fallbacks for that provider/model at the learned target, while the normal no-413 `/v1/responses` fast path remains unchanged.

Like chat completions, Responses requests are routed by the public `model` ID. Fallback retries for unsupported `/responses` models stay within the selected provider; the proxy does not silently switch providers.

For responses-only Azure deployments such as the `gpt-5.4-pro` example configuration, this is the canonical inference path.

Streaming Responses requests preserve upstream headers and are otherwise passed through directly. One narrow exception exists for `POST /v1/responses` with `stream: true`: if the first semantic SSE event is an immediate transient `response.failed` admission error such as `too_many_requests`, `model_overloaded`, `bad_gateway`, or `gateway_timeout`, the proxy translates that pre-commit failure into a normal HTTP error before flushing `200 OK`. All other streaming failures stay passthrough SSE.

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
- upstream requests use the proxy streaming timeout rather than websocket/client-disconnect cancellation
- history is stored append-only in memory
- long sessions are auto-compacted into one proxy-owned checkpoint plus a recent tail
- optional turn-state delta replay can be enabled with `--responses-ws-turn-state-delta`
- if upstream rejects delta replay, the proxy automatically falls back to full replay
- if the first streamed upstream event is a transient `response.failed` admission error, the bridge sends a wrapped websocket error frame instead of relaying the raw `response.failed` event

This websocket bridge is a proxy transport adaptation layered over upstream HTTP `/responses`. It is not the same feature as provider-native websocket or realtime APIs such as Azure `/realtime`.

See [responses-websocket.md](responses-websocket.md) for tuning flags.

## `POST /v1/responses/compact`

Compatibility shim for environments expecting `/responses/compact`. The proxy rewrites the request into a normal upstream `/responses` call with a compaction prompt, then returns:

- retained `system`, `developer`, and `user` messages from the original compact request
- one proxy-owned opaque `compaction` item whose `encrypted_content` can later be sent back to `/v1/responses` or `/v1/responses/compact`

Requests to this endpoint are accepted up to `64 MiB` so large session histories can be compacted without tripping the default request-body limit. If the upstream `/responses` call still rejects the compact payload with `413 Payload Too Large`, the proxy retries by compacting the input in chunks, including splitting oversized individual input items or string inputs into synthetic historical-context chunks before merging the partial summaries into one final checkpoint. During this fallback only, oversized fixed fields such as large `tools` arrays or `text` schemas may be omitted from retry requests when needed to stay under upstream payload limits. The chunk target body size starts at `4 MiB` (sized below the empirically observed Copilot `/responses` cap of `5 MiB`; configurable via `--compact-upstream-chunk-bytes` / `COMPACT_UPSTREAM_CHUNK_BYTES`) and is halved on each recursive `413` (and eagerly halved when a sub-target body is itself rejected) down to a `64 KiB` floor; once the floor is reached the original `413` is returned to the caller. Total logical compaction calls across the chunk fanout are capped by `--compact-upstream-max-attempts` / `COMPACT_UPSTREAM_MAX_ATTEMPTS` (default `48`, sized to the 64 MiB inbound ceiling with one round of recursive halving) to bound per-request cost; each logical call may add one extra HTTP POST when the configured model is unsupported (model-fallback) and is also subject to the shared transport-retry policy on transient upstream failures, so this cap is a fanout safety net rather than a precise HTTP-POST limit. Once the proxy has resolved a fallback model on the first chunk, subsequent chunks reuse it directly and skip the model-fallback probe. Budget exhaustion surfaces the original `413`. If the inbound request has `input:[]` and the upstream still rejects it, the original `413` is returned rather than a fabricated empty checkpoint.

If the requested model does not support the upstream Responses API, the proxy retries against a compatible fallback model discovered from `/models`.
That fallback stays within the selected provider; the proxy does not silently switch providers.

## `POST /v1/memories/trace_summarize`

Compatibility shim that summarizes one or more traces into `{trace_summary, memory_summary}` objects using the upstream `/responses` endpoint plus a JSON-only summarization prompt.

Requests to this endpoint are accepted up to `64 MiB` so larger trace bundles can be summarized in a single call.
