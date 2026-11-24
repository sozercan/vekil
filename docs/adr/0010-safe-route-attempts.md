# 0010: Require proof before replaying a routed request

## Context

An upstream rejection, a failure to read its response, and a response that committed billable work can look alike at the HTTP boundary. An automatic second send can create duplicate work or disclose state to a different owner.

## Decision

- Keep public model identity separate from operational routes, targets, and deployment models. Resolve the exact target and authentication owner at dispatch, and make provider-specific request rewrites only after selection.
- Permit failover only with positive evidence of a retry-safe rejection, within the route's policy and budgets. Failed or incomplete reads are not proof of rejection. Do not follow redirects as an implicit resend. A client-owned retry path must see the provider's actual status rather than a synthesized one.
- Buffer only enough to decide whether an event is a retryable failure. An oversized or malformed fragment may supply best-effort usage, but cannot certify rejection or change status; a partial result carrying visible output or usage is not permission to switch targets.
- Keep the provider's retry/reset value distinct from the proxy's bounded sleep policy. Admit a single recovery probe when a Copilot cooldown expires; other requests wait for its outcome rather than creating a recovery stampede. Drain abandoned bodies within a time/byte budget and keep one reader in charge so accounting and connection reuse do not introduce an unbounded wait.

## Consequences

Changing a model route does not automatically make new transports eligible to replay traffic. Retain attempt provenance and wasted usage even when nothing from the abandoned attempt reaches the client. See [attempt execution and replay safety](../architecture.md#attempt-execution-and-replay-safety).
