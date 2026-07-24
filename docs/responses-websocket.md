# Responses WebSocket Bridge

These settings affect the Codex-style `GET /v1/responses` websocket bridge. The bridge is disabled by default; when disabled, websocket upgrade attempts receive `426 Upgrade Required` so Codex-style clients can fall back to HTTP `/v1/responses`.

Schema-v2 policy profile IDs are unsupported on this bridge in v1. Policy selection begins only on the public OpenAI Chat surface, although the selected internal terminal route may use process-owned Chat-over-Responses. Policy profiles provide no websocket first-turn selection, affinity, or shared session state. Use a direct Responses-capable public model/route for websocket clients; its exact-target pinning rules below remain unchanged.

Important:

- This websocket bridge is proxy-owned and still forwards upstream over HTTP `/responses`.
- It is separate from Azure OpenAI's native `/realtime` websocket and WebRTC APIs.
- For a schema-version-2 route, the first provider-backed `response.create` may try the next ordered target only after a definitely replay-safe rejection and before websocket metadata, a Responses event, semantic output, or provider-bound state is exposed.
- Once a target is committed, the session is pinned to that exact `{route_id, target_id}`. Every later turn, delta replay, full-replay fallback, automatic compaction call, and protocol recovery stays on that target. Cross-target session migration is not implemented.
- If the pinned target becomes unavailable, the turn fails closed. Vekil does not replay the session to another route target, even when the route is configured with `priority_failover`.
- Each websocket session is serialized: one active turn is processed at a time, with at most one additional request queued. Vekil does not multiplex turns or implement Copilot-style request superseding; clients that try to queue more than one request receive a WebSocket policy-violation close.
- A dedicated session reader continues handling close and pong control frames while inference or automatic compaction is in progress. Client close/read failure, missed pong deadlines, the shared turn deadline, and server shutdown cancel the active upstream inference or compaction promptly, close retry admission, and prevent a secondary target or other new turn work from starting.
- Each provider-backed `response.create` is one logical operation with one target-attempt budget and one physical upstream-send budget. Initial selection, any safe first-turn failover, replay/compaction child calls, and same-target protocol recovery share those bounds; attempts remain serialized.
- Explicit-route websocket operation IDs use one connection ID plus a monotonically increasing turn sequence and are exposed as `X-Vekil-Request-ID` metadata/error headers.
- Response IDs, trusted `X-Codex-Turn-State`, opaque encrypted/session artifacts, and other adapter-marked state are bound immediately before exposure. Known state and committed session history require the exact session target; unknown or conflicting explicit-route state is rejected locally.
- `response.completed` and `response.incomplete` are terminal for the current upstream SSE response. After forwarding and recording the response ID, output, and usage, Vekil closes the upstream body immediately; trailing bytes, a held-open body, or a later transport reset do not delay the turn. Incomplete responses preserve partial output and may be continued with their response ID. `response.failed`, top-level Responses `error` events, and streams that end without a terminal response remain errors. A classified `response.failed` or top-level `error` that is the first semantic event is replaced by the bridge's wrapped error frame. An unclassified first `response.failed`, or any failure after another semantic event has already been relayed (including `response.created`, `response.in_progress`, or output), keeps the upstream event and is then followed by the wrapped frame.
- Dashboard client accounting is per provider-backed client `response.create`: a structurally valid terminal provider outcome is recorded immediately and exactly once; a client-first abort without a terminal outcome is recorded once as 499, while shutdown-first lifecycle cancellation is excluded. Physical sends and target switches are recorded separately. Tokens spent by proxy-internal auto-compaction or replay `413` compaction are added to that create's usage rather than recorded as synthetic client requests, including usage collected before a later compaction parse/merge failure. Post-terminal auto-compaction amends the existing record without delaying its request/status visibility. Failed provider turns keep terminal partial usage and remain attributed to the public model, final provider, and original agent; target-level public stats expose attempt counts rather than per-attempt token usage.

Target/state bindings are process-local, capped at 262,144 entries, and use a 24-hour absolute TTL. Websocket session history also lives in the process that owns the connection. Use one Vekil process or sticky ingress, and drain sessions before rollback or downgrade. A process restart or another replica cannot recover the live session or its binding map. Durable/shared bindings, signed target hints, and cross-target full replay are not implemented.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--responses-ws-enabled` | `RESPONSES_WS_ENABLED` | `false` | Enable the proxy-owned Codex websocket bridge on `GET /v1/responses` |
| `--responses-ws-turn-state-delta` | `RESPONSES_WS_TURN_STATE_DELTA` | `false` | Experimental: if the pinned upstream returns `X-Codex-Turn-State`, try replaying only the newest delta input on the next turn; rejection falls back to full replay on the same target |
| `--responses-ws-disable-auto-compact` | `RESPONSES_WS_DISABLE_AUTO_COMPACT` | `false` | Disable automatic session-history compaction |
| `--responses-ws-auto-compact-max-items` | `RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS` | `8` | Auto-compact when history item count exceeds this threshold |
| `--responses-ws-auto-compact-max-bytes` | `RESPONSES_WS_AUTO_COMPACT_MAX_BYTES` | `32768` | Auto-compact when raw history byte size exceeds this threshold |
| `--responses-ws-auto-compact-keep-tail` | `RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL` | `4` | Keep this many most recent items verbatim after websocket auto-compaction and as the starting tail for replay-like `POST /v1/responses` 413 compaction; the replay fallback aligns tool/call boundaries and may halve this per request if compacted retries still return 413 |
| `--compact-upstream-chunk-bytes` | `COMPACT_UPSTREAM_CHUNK_BYTES` | `4194304` | Initial target body size for chunked compaction retries after an upstream `413` (`/v1/responses/compact` and `POST /v1/responses` replay compaction). Default sized below the empirically observed Copilot `/responses` payload cap of `5 MiB` (`5,242,880` bytes); `4 MiB` leaves headroom for proxy-owned overhead. Halved on each recursive `413` (and eagerly halved when a sub-target body still 413s) down to a `64 KiB` floor. Learned smaller targets are cached in memory for 30 minutes per provider/model/endpoint so later compaction fallbacks skip known-doomed larger chunks; tune up for providers like Azure that accept larger bodies, or down if your upstream's payload cap is below `4 MiB` |
| `--compact-upstream-chunk-concurrency` | `COMPACT_UPSTREAM_CHUNK_CONCURRENCY` | `4` | Maximum number of sibling chunk compaction calls to run concurrently after the first chunk succeeds. Explicit model routes serialize these sends to preserve attempt cleanup and target pinning. Lower this to reduce upstream burst pressure; raise it only for legacy routes whose upstream can safely handle more parallel `/responses` compaction calls |
| `--compact-upstream-max-attempts` | `COMPACT_UPSTREAM_MAX_ATTEMPTS` | `48` | Maximum logical compaction calls a compact/replay `413` fallback may issue per inbound request. Each call may add one extra HTTP POST when the configured model is unsupported (model-fallback) and is independently subject to the shared transport-retry policy on transient upstream failures (429/502/503/504). Sized to the documented 64 MiB inbound ceiling at the default 4 MiB chunk target with one round of recursive halving plus headroom; meant as a runaway-fanout safety net rather than gatekeeping legitimate large requests or duplicating the transport retry budget. On exhaustion the original `413` is returned |

## Suggested Debug Run

```bash
./vekil \
  --log-level debug \
  --responses-ws-enabled \
  --responses-ws-turn-state-delta \
  --responses-ws-auto-compact-max-items 64 \
  --responses-ws-auto-compact-max-bytes 524288 \
  --responses-ws-auto-compact-keep-tail 16
```

With `--log-level debug`, websocket bridge logs include `delta_attempted`, `delta_fallback`, `auto_compacted`, `history_items`, `history_bytes`, and compaction before/after sizes.
