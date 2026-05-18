# Responses WebSocket Bridge

These settings affect the Codex-style `GET /v1/responses` websocket bridge. The bridge is disabled by default; when disabled, websocket upgrade attempts receive `426 Upgrade Required` so Codex-style clients can fall back to HTTP `/v1/responses`.

Important:

- This websocket bridge is proxy-owned and still forwards upstream over HTTP `/responses`.
- It is separate from Azure OpenAI's native `/realtime` websocket and WebRTC APIs.
- Each websocket session is serialized: one active turn is processed at a time. Vekil does not multiplex turns or implement Copilot-style request superseding. Closing the websocket ends the session; once Vekil observes the disconnect, it stops relaying and closes the upstream response body while the upstream request context remains governed by the proxy streaming timeout.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--responses-ws-enabled` | `RESPONSES_WS_ENABLED` | `false` | Enable the proxy-owned Codex websocket bridge on `GET /v1/responses` |
| `--responses-ws-turn-state-delta` | `RESPONSES_WS_TURN_STATE_DELTA` | `false` | Experimental: if upstream returns `X-Codex-Turn-State`, try replaying only the newest delta input on the next turn |
| `--responses-ws-disable-auto-compact` | `RESPONSES_WS_DISABLE_AUTO_COMPACT` | `false` | Disable automatic session-history compaction |
| `--responses-ws-auto-compact-max-items` | `RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS` | `8` | Auto-compact when history item count exceeds this threshold |
| `--responses-ws-auto-compact-max-bytes` | `RESPONSES_WS_AUTO_COMPACT_MAX_BYTES` | `32768` | Auto-compact when raw history byte size exceeds this threshold |
| `--responses-ws-auto-compact-keep-tail` | `RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL` | `4` | Keep this many most recent items verbatim after websocket auto-compaction and as the starting tail for replay-like `POST /v1/responses` 413 compaction; the replay fallback aligns tool/call boundaries and may halve this per request if compacted retries still return 413 |
| `--compact-upstream-chunk-bytes` | `COMPACT_UPSTREAM_CHUNK_BYTES` | `4194304` | Initial target body size for chunked compaction retries after an upstream `413` (`/v1/responses/compact` and `POST /v1/responses` replay compaction). Default sized below the empirically observed Copilot `/responses` payload cap of `5 MiB` (`5,242,880` bytes); `4 MiB` leaves headroom for proxy-owned overhead. Halved on each recursive `413` (and eagerly halved when a sub-target body still 413s) down to a `64 KiB` floor. Learned smaller targets are cached in memory for 30 minutes per provider/model/endpoint so later compaction fallbacks skip known-doomed larger chunks; tune up for providers like Azure that accept larger bodies, or down if your upstream's payload cap is below `4 MiB` |
| `--compact-upstream-chunk-concurrency` | `COMPACT_UPSTREAM_CHUNK_CONCURRENCY` | `4` | Maximum number of sibling chunk compaction calls to run concurrently after the first chunk succeeds. Lower this to reduce upstream burst pressure; raise it only when the upstream can safely handle more parallel `/responses` compaction calls |
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
