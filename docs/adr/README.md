# Architecture Decision Records

Use ADRs for non-obvious reasons, rejected alternatives, and consequences. Keep user-facing behavior in the relevant [product documentation](../README.md). Add a numbered record with Context, Decision, and Consequences; if a decision changes, mark the old record superseded and link its replacement.

| Record | Decision |
|---|---|
| [0001](0001-native-chat-responses-adapter.md) | Keep Responses-over-Chat on native Chat |
| [0002](0002-anthropic-wire-shapes.md) | Preserve distinct Anthropic wire shapes |
| [0003](0003-bounded-launch-logs.md) | Bound local launcher logs per file |
| [0004](0004-descriptor-based-macos-state-validation.md) | Verify macOS durable state through open descriptors |
| [0005](0005-dashboard-without-inference.md) | Keep the dashboard usable without model inference |
| [0006](0006-ephemeral-kubernetes-example-cache.md) | Keep the example Kubernetes token cache ephemeral |
| [0007](0007-separate-client-and-attempt-metrics.md) | Separate client traffic from upstream attempts |
| [0008](0008-linux-tray-fallback-boundaries.md) | Fall back on Linux only when the outcome permits it |
| [0009](0009-credential-and-launch-isolation.md) | Keep credential ownership separate from child processes |
| [0010](0010-safe-route-attempts.md) | Require proof before replaying a routed request |
| [0011](0011-responses-transport-commitment.md) | Commit Responses transport state only on the selected attempt |
| [0012](0012-state-ownership-and-history.md) | Separate exact state ownership from portable visible history |
| [0013](0013-canonical-chat-execution.md) | Translate through one canonical Chat execution seam |
| [0014](0014-reasoning-carrier-trust.md) | Separate portable reasoning from routing authority |
| [0015](0015-policy-admission-and-privacy.md) | Classify bounded facts before admitting terminal work |
| [0016](0016-desktop-and-server-lifecycle.md) | Give blocking platform interactions explicit lifetimes |
| [0017](0017-reproducible-smoke-gates.md) | Separate deterministic CI from credentialed provider smokes |
| [0018](0018-bounded-diagnostics-and-optimizers.md) | Bound diagnostic and optimizer access to request data |
