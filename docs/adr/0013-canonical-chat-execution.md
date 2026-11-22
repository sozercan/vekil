# 0013: Translate through one canonical Chat execution seam

## Context

Anthropic, Gemini, and OpenAI clients use different message, tool, and streaming shapes. A route may expose canonical Chat to clients but accept only Responses upstream. Separate translation and retry paths would drift in behavior.

## Decision

- Keep protocol handlers responsible for validation and projection at the public boundary. Execute a single canonical Chat operation, preferring native Chat before adapting to native Responses where the selected route allows it. Keep provider-specific fields out of other protocols and do not advertise emulated APIs as native endpoint capabilities.
- Translate Responses events once into typed internal events. Adapt to the client protocol from that stream instead of synthesizing Chat SSE only to parse it again. With tools, upstream streaming may be required even for a non-streaming client so concurrent tool calls can be reconstructed reliably.
- Fail explicitly on unsupported semantics instead of silently degrading them: the Responses-backed request path does not emulate non-empty `stop`, hosted tools, or unknown fields. Honor Chat's non-storage default, request encrypted reasoning internally for tool continuation, and retain ordering of the complete tool-call projection. Tool results may arrive in any order but must correspond to pending calls.
- Preserve raw provider request data where a targeted rewrite can be safely spliced. Never treat absent versus explicit values as interchangeable when those values determine provider compatibility, provenance, or a client's ability to replay.

## Consequences

Compatibility is deliberately asymmetric: [Responses-over-Chat](0001-native-chat-responses-adapter.md) is a separate opt-in, native-Chat-only adapter. The [API](../api.md) and [Responses](../responses.md) pages describe externally visible behavior.
