# 0011: Commit Responses transport state only on the selected attempt

## Context

HTTP, the local WebSocket bridge, and native Copilot WebSockets offer different continuation and retry semantics. A transient stream close or a provider-specific event must not publish abandoned state or manufacture a second turn.

## Decision

- Choose native WebSockets only on positive transport support. An established native continuation can have no equivalent HTTP request; do not automatically replay it on another transport. Full-history requests may fall back to HTTP when native transport cannot carry them. Evidence that a handshake or subprotocol is unsupported is distinct from a transient transport error.
- Keep upstream turn state and response headers attempt-local until selection. After commitment, preserve an authoritative terminal result that was already parsed, including one received as a timeout or close races delivery. Do not publish a spurious error frame during lifecycle cancellation or rewrite a failure into apparent client success.
- Keep automatic bridge compaction on the HTTP path. Deduplicate equivalent in-flight compactions across waiters only within the same provider/authentication and request boundary. Bound response buffers, chunk fanout, and output tokens; if fallback from a known upstream failure does not succeed, preserve its authoritative status and bounded diagnostics instead of substituting the fallback's error.
- Rewrite public model identity and streamed citation markers only on the applicable response shapes. At stream boundaries, carry ambiguous partial marker text forward until it can be classified; do not change unrelated event payloads or make malformed data terminate passthrough.

## Consequences

Transport preference does not imply permission to replay, and shared compaction does not imply shared authorization. See [Responses compatibility](../responses.md) and the [WebSocket bridge](../responses-websocket.md).
