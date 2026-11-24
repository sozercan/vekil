# 0007: Separate client traffic from upstream attempts

Status: Accepted

## Context

One inference request can produce multiple upstream sends or late internal compaction. Counting those as client requests would distort latency and token averages and change the meaning of existing metrics. The dashboard also needs recent diagnostics without retaining provider payloads or unbounded user-supplied labels.

## Decision

- Count generating routes through a positive allowlist aligned with the server router. Exclude probes, catalog reads, the websocket upgrade, and proxy shims: they produce no comparable per-request completion. Match Gemini's generating action suffix, not just its route prefix. Record each websocket create turn directly, including failures and turns with no usage. Keep streaming durations out of the latency averages and percentiles: stream lifetime isn't comparable with a non-streaming response.
- Keep legacy same-target retries separate from explicit-route physical attempts, target switches, and route exhaustion. Status `0` in the legacy ledger means a transport error. An explicit retry-accounting marker is propagated when rebuilding an inference context from the lifecycle root; a context without the marker cannot add retries to client traffic metrics. Configured provider keys are not client-controlled, but client-controlled model and agent keys need both length and cardinality limits. Retain classified agent labels, not raw user agents.
- Count reported physical usage in the public prompt/completion vocabulary, and count usage from unaccepted attempts in the wasted subset. The attempt ledger is independent of its trace ring: reclassification or cleanup completion may arrive after eviction. Publish cumulative versioned snapshots so newer publications subsume older ones; `routeAttemptRecordState.mu` protects the attempt, while `statsCollector.mu` protects published counters and ring slots. Merge evidence without replacing stronger execution evidence with weaker preamble or terminal-only observations. The bounded trace exposes normalized outcomes and validated labels, not payloads, raw errors, upstream model rewrites, or state secrets.
- Attach pre-terminal internal compaction usage to the turn without replacing it during terminal usage normalization. Amend the original aggregates with late usage; check slot identity before updating a recent or time-series slot, and never add a second request, error, or latency sample. Sample client and auxiliary activity in scheduling order before reading the totals so a detached worker doesn't cause a premature idle observation.
- Use a lazily populated per-second ring with absolute-second tags and a zero-filled snapshot, without a background ticker. Compute nearest-rank latency percentiles in milliseconds from a bounded ring of recent samples. The collector mutex protects snapshots and all helpers that read or mutate those rings. Return the bounded union of leaders for the metrics that the browser can sort by, ordered by the default request sort. A requests-only cutoff would hide small models with disproportionately high errors or latency. Zero-valued metrics do not nominate leaders.

## Consequences

Client metrics and physical spend answer different questions and are not interchangeable. State storage gauges include recovered records independently of traffic. Changes to generating routes, accounting boundaries, redaction, or dashboard sort metrics must keep the collector and [dashboard contract](../dashboard.md) aligned.
