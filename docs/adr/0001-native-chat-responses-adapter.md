# 0001: Keep Responses-over-Chat on native Chat

Status: Accepted

## Context

Some providers expose only Chat Completions, while clients such as Codex submit Responses requests. Vekil already adapts in the opposite direction for Responses-only backends. Allowing one adapter to invoke the other could loop, and forwarding backend-specific reasoning or tool replay identifiers would silently lose state. Policy public IDs already have a separate classifier-backed Responses contract.

## Decision

- Opt in per public model ID, prefer native Responses where available, reject policy and internal IDs, and keep native endpoint metadata unchanged. Request admission, launcher selection, and Codex catalog projection use the same route-bound decision. For an explicit route, its validated endpoint contract applies to all failover targets. Look up internal route IDs only after public IDs and aliases miss.
- Derive `vekil_responses_compatibility` from the local eligibility decision. Strip upstream copies of the marker before projecting the catalog, so even a chained Vekil cannot opt a model in on behalf of this proxy. Do not inherit provider-specific speed tiers or inject a default `none` reasoning level for Chat-backed models with no advertised reasoning controls.
- Share bounded request translation with policy ingress, but use the ordinary Chat request ceiling rather than the smaller policy ceiling: full Codex tool catalogs and long histories need more room, and Chat wrappers and tool aliases can expand the translated body. Return unsupported reasoning levels as errors rather than guessing equivalent values; `none` without any reasoning capability is not an override.
- Require native Chat again at execution, before any send. A catalog refresh can invalidate the admission decision; in that case the client can retry, but the proxy cannot switch backends. Reject replay identifiers that only a Responses backend can restore.
- Force upstream streaming for parallel tool-call reliability and run the same state machine for incremental SSE and aggregate JSON. Terminate the lifecycle-rooted upstream read on client cancellation rather than waiting for a failed client write.
- Track each Chat tool index independently. A name may arrive in multiple fragments, including repeated fragments; neither other calls nor intervening text proves it is complete. Announce a call when its arguments start or at successful finish, fix its identity once announced, and tolerate only an exact repeated full name thereafter. Assign monotonically increasing output indexes in exposure order, not Chat index order. Return items without those indexes so a full-history client can replay an `output_item.done` item verbatim. Text after an announced call opens another message item; coalesce the assistant items when translating that history back to Chat.
- Emit the executable tool completion event only after validating the terminal state and JSON arguments. A malformed delta may have corrupted a tool argument and must fail the turn, not be skipped. Gemini through Copilot may end complete calls with `stop`. A valid finish reason is required, but the subsequent `[DONE]` sentinel is not. Omit `none` or `auto` tool choice when Codex sends it without tools on a local compaction request; Copilot rejects that shape. Leave usage null if the upstream doesn't report it.
- Return a terminal failure as an HTTP error before writing and as `response.failed` after committing the SSE stream. Preserve a client cancellation as 499 in statistics even when the status on the wire is already 200. Record truncation as an unsuccessful semantic outcome, as for native `response.incomplete`.

## Consequences

Compatibility does not make Chat-only models natively Responses-capable. The adapter supports replay of its visible, completed output through full history, not cross-backend reasoning continuity or provider state. The public contract and bounds are in [Responses](../responses.md#responses-compatibility-over-a-native-chat-model-responses-over-chat).
