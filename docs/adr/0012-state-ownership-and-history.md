# 0012: Separate exact state ownership from portable visible history

## Context

A provider state identifier is not a portable conversation. Visible history can sometimes be reproduced on another target, but provider-private reasoning, compaction, partial tools, and an interrupted handoff can make replay unsafe.

## Decision

- Bind state to the exact provider, target, and authenticated owner, committing proof before exposing the binding to clients. Validate and bind against one consistent view. Store durable proofs for routes that opt into durable ownership; do not expire, evict, or silently repair authoritative records during serving.
- Keep visible-history migration opt-in. History is branch-aware and records complete visible items, not provider-private reasoning. Never infer a shared branch from matching text or redirect a previously issued provider response ID. A supplied transcript must agree with the known lineage; prefer newer complete evidence over a stale anchor.
- Forward encrypted reasoning only to its proven owner. To switch owners, construct a request from complete portable history with the old owner identifiers removed. If the source cannot be reconstructed or has unsafe pending effects, use the exact owner or fail closed, not a partial replay.
- Treat a tool call as handed off only at its client-delivery boundary. Stage stream items until their completion and delivery are established; a buffered delta or an item that cannot itself invoke a tool is not proof of tool execution. Distinguish client cancellation from proxy shutdown, and keep unresolved attempts blocking until the corresponding history can be retired safely.

## Consequences

Durable binding and migration have independent guarantees. Recovery and offline pruning must preserve unresolved attempts and history as a unit, and pruning does not promise immediate file shrinkage. See [state recovery](../state-recovery.md) and [conversation migration](../conversation-migration.md).
