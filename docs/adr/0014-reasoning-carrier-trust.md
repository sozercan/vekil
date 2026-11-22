# 0014: Separate portable reasoning from routing authority

## Context

Claude Code can replay a proxy-issued thinking signature after the in-memory tool-replay store is evicted or the proxy restarts. That signature is client-controlled input and can appear in a request to another provider.

## Decision

- Preserve ordered reasoning placeholders and explicit visible-call bindings; positional inference misattaches same-name parallel calls. Use the client's visible transcript for text and tool content, and leave provider-owned call IDs in process-local state. A missing or rejected carrier degrades that turn independently to visible history when safe.
- Bind carrier restoration to the model/route that minted it. A restart-tolerant route digest is not authorization to choose a policy tier. Only a current-process tag authenticating the whole relevant payload can select a route; loss of that key fails closed for pinning without preventing safe restoration on an independently selected matching route.
- Budget decompression per carrier and across a request, and enforce a separate cumulative emitted wire budget. Otherwise long transcripts and multiple compressed inputs can exhaust resources even when each carrier looks small.
- Strip proxy-issued carriers before forwarding to native Anthropic. Detect their prefix after JSON decoding and inspect duplicate, escaped, and case-aliased fields without collapsing occurrences. A raw byte scan is not equivalent: escaped text can evade it while still reaching the upstream parser. Preserve unrelated raw fields and remove an assistant message left empty solely by stripping the carrier.

## Consequences

The carrier does not reauthorize private state, provider identifiers, or an upstream switch. An untrusted or oversized carrier cannot take down unrelated tool history or expand request bounds. Chat clients without this carrier still use process-local replay.
