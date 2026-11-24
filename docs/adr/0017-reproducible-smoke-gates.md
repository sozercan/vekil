# 0017: Separate deterministic CI from credentialed provider smokes

## Context

Live providers and CLIs are not reproducible test dependencies. At the same time, credential handling, model discovery, and provider failover need coverage at their real integration boundaries.

## Decision

- Keep the deterministic core gate independent of upstream credentials. For credentialed smokes, neutral-skip only when preconditions such as secret availability are absent; once configured, report provider and routing regressions as failures. Never run PR-controlled code with a privileged base-repository workflow context.
- Exercise ordered failover with a bounded local control proxy around the selected provider, not by passing arbitrary live traffic or assuming a real provider will produce a retry-safe error on demand. Validate static config before live sends, pin the focused gate to controlled inputs, and keep independently credentialed cross-provider validation manual.
- Match legacy Copilot model eligibility to routing: an empty endpoint catalog is not evidence that Chat is unsupported. Require explicit Responses support and effort capabilities when a focused Responses test depends on them.
- Pin workflow actions by commit and label each pin with its exact release tag when available so reviewers can identify the version without resolving the SHA. Use bounded deadlines and readiness signals for harness startup and cleanup, and retain sanitized diagnostics without printing secrets or private config.

## Consequences

An upstream model failure is distinguishable from a missing prerequisite or a defect in the harness. The repository's workflow matrix and operator setup remain in [Development](../development.md#ci).
