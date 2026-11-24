# 0015: Classify bounded facts before admitting terminal work

## Context

Semantic routing can send parts of a request to a separate classifier and change the selected destination. This cannot be treated as a background inference with the same privileges as a terminal request.

## Decision

- Validate the full request and tool-history contract before classifier admission. Keep classifier routes internal with one target and one send; do not recirculate a policy request through a listening proxy. Accept only the bounded typed classifier result, then map it deterministically to a configured tier.
- Send only bounded, declared facts. Content forwarding needs explicit acknowledgement and trust/retention checks; a no-store request field does not establish provider retention behavior. Do not log prompts, tool arguments, reasoning, client content, credentials, or diagnostic values that reproduce them.
- Keep `off`, `observe`, and `enforce` separate. In observe, classifier work cannot control the actual terminal send; preserve baseline behavior and report the result as a counterfactual. In enforce, a fallback must stay within the admitted trust and tier rules. The process-wide override may lower a profile's configured mode, not raise it.
- Carry effort from the policy's selected tier, not from the classifier or an arbitrary incoming effort. Bind a stateful continuation to proven ownership rather than silently reclassifying it onto an incompatible target.

## Consequences

Policy provenance can be useful without carrying user content. Internal route IDs are operational metadata and never become public aliases. See [Semantic Policy Routing](../policy-routing.md) for rollout, failure handling, and single-tenant limits.
