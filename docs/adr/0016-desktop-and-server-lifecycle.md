# 0016: Give blocking platform interactions explicit lifetimes

## Context

Desktop portal replies may race subscription and cancellation. Authentication, inference, and interactive UI also have different lifetimes from the local listener.

## Decision

- Subscribe to portal Request responses before issuing the method call. Scope to the caller's request namespace and accept either the predicted path or returned handle. Prefer a matching response already queued on cancellation; otherwise close the request best-effort with an independent bounded context.
- Give file chooser and URI requests their own D-Bus peers. Share the notification peer for its process lifetime and replace it on transport failure without closing a live peer used by other confirmations. Scope notification signals by ID and require an explicit action; an ambiguous delivery error is not permission to show a second prompt. See [ADR 0008](0008-linux-tray-fallback-boundaries.md).
- Run portal waits off the tray dispatch loop. Keep the listener's generation and shutdown ownership explicit: cancel lifecycle-rooted inference before draining existing sessions, fence late results, and do not let callbacks belonging to a previous server generation affect a restart.
- Keep listeners viable during authentication failure, but do not advertise readiness before auth and required configuration are usable. Where platform-specific process supervision is needed, keep its resources scoped to the launch or service rather than the entire desktop session.

## Consequences

Cancellation tests should simulate the relevant boundaries rather than depend on a real desktop session or the scheduler. UI fallback and server restart are independent; neither may duplicate work that already escaped the process.
