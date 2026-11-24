# 0018: Bound diagnostic and optimizer access to request data

## Context

Request accounting, malformed-stream diagnostics, and optional tool optimizers need enough context to explain or improve a request without changing the ordinary data path or disclosing client content.

## Decision

- Keep the physical attempt and public request accounting separate as in [ADR 0007](0007-separate-client-and-attempt-metrics.md). Count billable partial usage from abandoned attempts without letting a diagnostic parser redefine the outcome. Bind operational provenance to the dispatch boundary; do not let an upstream-supplied identifier stand in for a proxy-generated one.
- Record only proxy-created path segments and bounded, allowlisted error classifications. A provider can echo unknown field names and other client values back; those values are not safe merely because they came from an error object. Unrecognized details may reduce precision, not correctness or privacy.
- Keep optional tool optimizers disabled by default and fail open on errors, timeouts, invalid output, or invalid replacements. Scope tool output context to the associated call and maintain bounded eviction order; do not change an original request when the optimizer cannot prove a valid replacement.

## Consequences

Dashboard metrics remain useful when detailed probes or optimization fail. See [Dashboard](../dashboard.md) and [Tool Optimizers](../tool-optimizers.md).
