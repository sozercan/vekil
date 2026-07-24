# Semantic Policy Routing

Semantic policy routing lets one public canonical-Chat model ID choose between a `lightweight` and a `powerful` terminal route for each root request. It is a schema-version-2 feature layered above Vekil's provider-agnostic route executor: the policy chooses one terminal route, then the existing executor owns target ordering, provider auth, model rewriting, safe physical failover, streaming, and response normalization.

The first release is intentionally narrow. Treat the limits and operator gates in this document as part of the public contract, not as temporary suggestions.

## Product boundary

Policy routing is cooperative quality/cost optimization. It is **not**:

- an authorization boundary;
- a spending or quota enforcement mechanism;
- an approval system for destructive tools or external actions; or
- a replacement for downstream identity, authentication, or tenant isolation.

A client can bypass a policy by requesting any exposed terminal route or other public model directly. If clients must not select the destination models, configure those routes with `exposure: internal`. Internal exposure removes their public aliases and catalog entries; it does not add authentication to Vekil.

The selectable v1 tiers are only `lightweight` and `powerful`. `versatile` remains catalog/picker metadata and is not a third routing tier.

## Locked v1 scope

A policy public ID is supported for:

- `POST /v1/chat/completions`;
- translated `POST /v1/messages` and `/v1/messages/count_tokens`;
- bounded stateless `POST /v1/responses` compatibility used by Responses-only agents;
- text-only canonical Chat messages and standard function tools;
- Responses namespace tools flattened to deterministic function aliases;
- OpenAI-family terminal routes served by native `/chat/completions` or bounded Chat-over-Responses;
- one per-turn decision with no general affinity or session cache, except process-local replay that remains bound to its originating route and tier; and
- the built-in `coding_agent_v1` classifier profile.

The policy can run in `off`, asynchronous `observe`, or synchronous `enforce` mode. One root request resolves one policy profile and selects one terminal route. Physical failover, if configured, stays inside that selected route.

The following are explicitly unsupported for policy public IDs in v1 and fail locally rather than silently bypassing classification:

| Surface or request shape | v1 policy behavior |
|---|---|
| `POST /v1/responses` | Bounded stateless compatibility: `store` false/omitted, no `previous_response_id`, text plus function/namespace-child tools; output is adapted from the selected Chat terminal |
| Proxy-owned `GET /v1/responses` websocket bridge | Unsupported |
| `POST /v1/responses/compact` | Unsupported |
| `POST /v1/memories/trace_summarize` | Unsupported |
| Responses-backed Chat terminal routes | Supported through the bounded Chat-over-Responses adapter; destination and classifier routes may expose `/responses` |
| `POST /v1/messages` and `/v1/messages/count_tokens` | Translated to canonical Chat, planned by the policy, and translated back to Anthropic |
| Gemini `generateContent`, `streamGenerateContent`, and `countTokens` routes | Unsupported |
| Image, audio, file, or any other non-text Chat input | Unsupported |
| Hosted/custom tools | Unsupported; policy Responses accepts only function tools and namespace children that are functions |
| Sticky affinity, policy session headers, or shared policy state | Unsupported; opaque downstream replay IDs are accepted only in `off`/`observe` for a single-target baseline |
| Multi-tenant policy deployments | Unsupported |

Direct public models and direct exposed terminal routes retain their existing endpoint behavior. `/v1/models` keeps a policy profile's public metadata at `/chat/completions`; translated Anthropic, policy Responses compatibility, and an internal Responses-backed terminal do not change that public contract. When a request may execute through Responses-backed Chat, the strict shared request subset is validated before classifier admission so requests guaranteed to fail never forward classifier content.

Policy Responses compatibility is deliberately not near-zero-copy passthrough. It converts bounded Responses input into canonical Chat, applies policy planning, executes the selected native or Responses-backed Chat terminal, aggregates the terminal result, and emits Responses JSON/SSE. Bounded `text.format` values are mapped to Chat `response_format`, including Codex `--output-schema` JSON schemas. It accepts Codex-style full stateless history, including prior `function_call` plus `function_call_output` items. Namespace children are flattened to deterministic names of at most 64 characters and mapped back to `namespace` plus `name` in the returned function call. Completed terminal output is checked against `tool_choice`; required or forced choices cannot complete without a matching function call. The launcher disables hosted web search, remote compaction, freeform apply-patch, Responses Lite, code-only tool modes, and inherited speed tiers for policy-owned Codex models. Deferred tool discovery remains unsupported: `defer_loading: true` and `tool_search` fail locally instead of being silently flattened or ignored. The adapter accepts large direct function catalogs for downstream Responses-backed Chat bridges. Native OpenAI/Azure Chat destinations may impose a 128-function limit, so deployments using those terminals must constrain the client catalog accordingly.

When Vekil itself owns a Responses-backed terminal, `call_vekil_*` state records the originating route and policy tier, so same-process continuations remain pinned even in `enforce`. A downstream Chat-compatible bridge may also return process-local replay IDs; those continuations remain limited to an `off`/`observe` baseline with one target and require a single bridge instance or sticky ingress to the replay-owning process. One configured target proves route determinism, not replica affinity.

Native Chat tool history must be complete and internally consistent before classifier admission. Assistant tool-call IDs must be unique, every tool result must reference one pending prior call exactly once, and all pending calls must receive results before the next non-tool message. Parallel results may arrive in any order. Malformed, missing, unknown, or duplicate tool-call relationships fail locally with no classifier or terminal-model send.

## Quick start

Start from [`examples/policy-routing-coding-economy.yaml`](../examples/policy-routing-coding-economy.yaml), replace the placeholder URLs/models, and export the referenced credentials.

For a single-process GitHub Copilot setup, use
[`examples/policy-routing-copilot.yaml`](../examples/policy-routing-copilot.yaml).
Its pinned Copilot Responses targets are adapted to canonical policy Chat in
process, including classifier execution, so no second Vekil listener is needed:

```bash
vekil launch claude \
  --model gpt-5.6-semantic \
  --providers-config examples/policy-routing-copilot.yaml
```

Offline validation performs strict decoding and local contract checks without sending classifier traffic:

```bash
vekil config validate \
  --providers-config examples/policy-routing-coding-economy.yaml
```

Live validation additionally sends one fixed, non-user preflight fixture through each distinct classifier route selected by the policy config:

```bash
vekil config validate --live \
  --providers-config examples/policy-routing-coding-economy.yaml
```

Run on loopback. With the default `--policy-routing=config`, each profile follows its YAML `mode`:

```bash
vekil \
  --providers-config examples/policy-routing-coding-economy.yaml
```

Use `--policy-routing=observe` for an explicit process-wide rollout cap, or `--policy-routing=off` as an emergency downgrade. An explicit process ceiling can lower a profile's configured mode but cannot raise it. For a GitHub-authenticated Copilot example with direct Responses-backed destinations, see [`examples/policy-routing-copilot.yaml`](../examples/policy-routing-copilot.yaml).

> **Single-tenant warning:** v1 policy `observe` and `enforce` support one trusted user or tenant per Vekil process/deployment. Loopback is the default supported topology. A non-loopback bind requires `--policy-routing-allow-remote-single-tenant` or `POLICY_ROUTING_ALLOW_REMOTE_SINGLE_TENANT=true`. That acknowledgement adds no authentication or tenant isolation; use a trusted external access layer and do not expose the port publicly.

## Schema-version-2 model

Schema v2 separates two registries:

- The **public model-entry registry** resolves exact public model IDs and supported normalized aliases to either a static route or a policy profile.
- The **terminal route registry** contains public and internal operational routes. Operational provider, route, target, and deployment IDs are never public aliases.

A policy profile references terminal routes by operational `id`. It cannot reference another policy profile.

### Route exposure

For schema v2, `model_routes[].exposure` is `public` or `internal`; omission defaults to `public`.

- A public route requires `public_id` and may publish normal picker/catalog metadata.
- An internal route must omit `public_id`, `model_picker_enabled`, and `model_picker_category`.
- Internal routes are excluded from public aliases, client model resolution, `/v1/models`, dashboard insight-model selection, and public fallback behavior.
- Policy destinations may be public or internal.
- A classifier route must be internal and declare `internal_purpose: policy_classifier`.

The classifier route is an auxiliary route, not a destination tier. It must contain exactly one target and permit exactly one target attempt and one upstream send.

### Provider data metadata

Every provider referenced by a policy destination or classifier route requires `trust_domain`, an operator-defined data-governance label.

`classifier_no_store_supported: true` declares that Vekil's classifier request adapter can send a provider-supported non-storage option. Vekil sends that option whenever declared. If the classifier provider does not declare non-storage support, the profile must explicitly set `allow_provider_retention: true`.

These fields are operator acknowledgements and capability declarations. They do **not** prove the provider's external retention, training, or deletion behavior.

### Policy profile fields

A policy profile binds:

- a public identity: `id`, `public_id`, and optional `name`/picker metadata;
- the `lightweight_route` and `powerful_route` terminal IDs;
- `baseline_tier`, `classifier_unavailable_tier`, and `classifier_uncertain_tier`;
- one internal classifier route plus the built-in classifier profile and bounds; and
- mandatory data-policy acknowledgements.

The v1 defaults are economy-oriented:

- baseline: `lightweight`;
- classifier unavailable: `lightweight`, so classifier overload does not become a global expensive-mode switch; and
- classifier uncertain: `powerful`, so an ambiguous individual request preserves quality.

Unavailable and uncertain fallbacks are not cached in v1.

| Omitted setting | Default |
|---|---|
| profile `mode` | `off` |
| `model_picker_enabled` | `true` |
| `model_picker_category` | `versatile` |
| `baseline_tier` | `lightweight` |
| `classifier_unavailable_tier` | `baseline_tier` |
| `classifier_uncertain_tier` | `powerful` |
| classifier `profile` | `coding_agent_v1` |
| `timeout_ms` | `3000` |
| `max_completion_tokens` | `256` |
| `max_request_bytes` | `16000` |
| `recent_turns` | `4` |
| `max_concurrency` | `4` |
| `observe_sample_rate` | `1.0` |

Valid classifier profile ranges are:

| Field | Valid range |
|---|---|
| `timeout_ms` | `100..10000` |
| `max_completion_tokens` | `32..1024` |
| `max_request_bytes` | `1024..65536` |
| `recent_turns` | `0..8` |
| `max_concurrency` | `1..32` |
| `observe_sample_rate` | finite number in `[0, 1]` |

At most 128 policy profiles may be configured. Policy routing requires `schema_version: 2`; schema-version-1 files reject explicit-route and policy-routing fields. Existing route-only schema-version-2 configurations remain valid without `policy_profiles`, and their aliases, dynamic refresh, catalog output, retries, and YAML behavior remain unchanged.

Validation also rejects:

- public/operational ID collisions in their applicable namespaces;
- public metadata on an internal route;
- recursive policy references;
- destination routes without `/chat/completions` or `/responses` Chat execution support;
- unsupported provider families or dynamic providers other than pinned `type: copilot` targets;
- terminal routes with different preferred Chat backends or other public Chat request semantics;
- classifier routes that are public, have the wrong internal purpose, or can send more than once;
- classifiers that cannot perform the forced function-tool protocol;
- missing trust-domain or data-policy acknowledgements; and
- unsupported custom classifier prompts, custom classifier output schemas, arbitrary routing languages, or config hot reload.

## Profile mode and global ceiling

Each profile has `mode: off|observe|enforce`, defaulting to `off`. The process-wide setting is a safety ceiling:

```text
--policy-routing=config|off|observe|enforce
POLICY_ROUTING_MODE=config|off|observe|enforce
```

`config` follows the YAML profile exactly. An explicit `off`, `observe`, or `enforce` value acts as the process-wide ceiling:

| Process mode | Profile `off` | Profile `observe` | Profile `enforce` |
|---|---|---|---|
| `config` | off | observe | enforce |
| `off` | off | off | off |
| `observe` | off | observe | observe |
| `enforce` | off | observe | enforce |

This lets profiles graduate independently while preserving one emergency rollback: set `POLICY_ROUTING_MODE=off` to send every policy request to its baseline tier with zero classifier calls.

### Off

- Dispatch `baseline_tier`.
- During normal server startup/runtime, make no classifier or live-preflight sends. An explicit operator invocation of `config validate --live` is separate.
- Preserve direct-route behavior.

### Observe

- Build an immutable bounded fact snapshot before terminal dispatch.
- Dispatch `baseline_tier` immediately; classification never changes the response path.
- Deterministically sample by root operation ID plus policy ID.
- Attempt non-blocking admission against both global and per-profile classifier limits.
- If admitted, classify asynchronously on a lifecycle-rooted timeout detached from the response path.
- Maintain no observer queue or backlog.
- Record exact not-sampled, admission-drop, completion, failure, and decision reasons.

### Enforce

1. Build and validate canonical Chat facts.
2. Attempt global and per-profile classifier admission without waiting.
3. On admission or infrastructure-health failure, choose `classifier_unavailable_tier`.
4. On successful admission, classify synchronously.
5. On abstention or missing/malformed/schema-invalid output, choose `classifier_uncertain_tier`.
6. Otherwise apply the deterministic tier mapper.
7. Compile and seal the selected terminal route plan.
8. Execute only that route.

If root cancellation or lifecycle shutdown occurs during classification, Vekil returns cancellation and authorizes no terminal-model send.

## Classifier facts and privacy

Classifier facts are built before tool-output optimization and contain bounded user/system content. Vekil includes:

- system/developer anchors, capped at 2,000 UTF-8 bytes total;
- the first user task, capped at 4,000 UTF-8 bytes;
- up to `recent_turns` recent non-anchor text messages, each capped at 1,500 UTF-8 bytes;
- function-tool names only, capped at 128 UTF-8 bytes each and 128 tools total;
- typed message, tool, and context counts; and
- original byte counts and truncation flags.

The serialized canonical facts JSON is capped at `max_request_bytes`; the fixed forced-tool Chat envelope is separately bounded by the implementation.
 Policy requests larger than 1 MiB are rejected before fact materialization so classifier admission cannot be bypassed with oversized message/tool arrays.

Vekil excludes provider credentials, inbound authorization, provider state, replay IDs, session identifiers, raw routing metadata, physical deployment names, function parameter schemas, and tool arguments. Classifier decisions and metrics also exclude prompt text and raw model output.

No general redaction algorithm can guarantee that secrets embedded in user content are removed. Therefore:

- `content_forwarding_acknowledged: true` is mandatory;
- classifier and both destination providers must share a `trust_domain` unless `allow_cross_trust_domain: true`; and
- retention must be explicitly acknowledged when the classifier provider cannot accept the configured non-storage request behavior.

Use `allow_cross_trust_domain` and `allow_provider_retention` only after an operator review. They are acknowledgements of risk, not security controls.

## Classifier protocol and deterministic mapping

The `coding_agent_v1` adapter forces exactly one strict function call named `emit_policy_signals`. It sends no other classifier tools and rejects duplicate JSON keys, missing or extra fields, invalid enums/integers, and trailing content. The signals are:

- `abstain`: boolean;
- `turn_type`: `chitchat`, `lookup`, `execution`, `exploration`, `edit`, `planning`, `debug`, `review`, or `other`;
- `code_scope`: `none`, `single_line`, `function`, `file`, `multi_file`, `cross_module`, or `unknown`;
- `tool_call_count_estimate`: integer `0..128`;
- `modifying_tool_call_count_estimate`: integer `0..128`;
- `requires_codebase_context`: boolean; and
- `risk_level`: `low`, `medium`, or `high`.

V1 deliberately excludes model-reported confidence and a model-recommended tier.

After fallback precedence, the mapper selects `powerful` when any of these is true:

- `turn_type` is `planning`, `debug`, `review`, or `exploration`;
- `code_scope` is `multi_file`, `cross_module`, or `unknown`;
- `risk_level` is `high`;
- `modifying_tool_call_count_estimate >= 2`;
- `requires_codebase_context` is true; or
- the local fact builder truncated task/context content.

Otherwise it selects `lightweight`. The built-in classifier calibration treats an explicit low- or medium-risk edit bounded to one file or one function as `edit` with `file`/`function` scope and no broad codebase-context requirement unless the request actually depends on cross-file or cross-module information. Inspecting the named target and nearby lines does not by itself make the request codebase-wide.

Classifier output is advisory data, not a trusted control plane. Malformed or adversarial content can affect only the current request's uncertain fallback; it cannot authorize actions, change another request's route, consume unbounded capacity, or open an infrastructure breaker.

## Admission, breaker, and fallback safety

Classifier capacity is queue-free and non-blocking:

- process-wide classifier concurrency is fixed at 32 in v1;
- per-profile concurrency defaults to 4 and is bounded by `max_concurrency`;
- a request must acquire both limits; and
- partial admission is released immediately if the second limit is unavailable.

There is one infrastructure breaker per classifier route. Only pre-inference connection/TLS/request-construction transport failure, `429`, and upstream `5xx` affect shared breaker state.

Defaults:

- open after five consecutive infrastructure failures;
- 30-second cooldown;
- an authoritative `429 Retry-After` opens immediately, capped at 60 seconds;
- one half-open probe; and
- any successful classifier HTTP exchange closes the breaker, even if its semantic payload is uncertain.

A classifier-local timeout uses `classifier_unavailable_tier` for that request but does not alter shared breaker state. Admission drops, malformed output, missing forced calls, abstention, surprising signals, and user validation errors also never affect the breaker.

Classifier sends have their own timeout, concurrency admission, operation, and one-send budget. They do not consume the selected terminal route's target-attempt or upstream-send budgets.

After selection, all terminal failure behavior remains inside the chosen route. A Responses-backed tool-result continuation first resolves its process-local replay owner and remains bound to the originating terminal tier/target; it does not run a new classifier decision that could migrate the replay state.

- a failed lightweight route never invokes powerful;
- a failed powerful route never downgrades;
- no cross-tier automatic fallback occurs;
- no quality retry occurs after semantic output or successful generation; and
- any replay-safe physical failover remains among equivalent targets already configured inside the selected terminal route.

## Live preflight and readiness

Endpoint metadata alone does not prove that a classifier accepts forced strict function output or non-storage request options.

When any profile's **effective** mode is `observe` or `enforce`, startup performs one live preflight per distinct classifier route using fixed non-user content. With the default process mode `config`, effective mode comes directly from each profile's YAML `mode`; explicit `off` or `observe` process ceilings remain available for rollback. It verifies:

- authentication and endpoint reachability;
- forced `emit_policy_signals` selection;
- strict argument-schema acceptance;
- acceptance of the configured non-storage behavior; and
- a maximum of one physical send.

Mode-specific behavior:

- `enforce`: failed preflight prevents readiness and startup completion;
- `observe`: failed preflight keeps the profile off and reports a readiness/configuration diagnostic rather than silently observing; and
- `off`: no live preflight or classifier send.

`vekil config validate` remains offline. Use `vekil config validate --live` when an operator wants the same protocol preflight without starting the server. A successful preflight proves protocol acceptance only; it does not prove external provider retention behavior.

## Catalog and output identity

A policy profile appears exactly once in `/v1/models` with:

| Catalog field | Derivation |
|---|---|
| `id` | profile `public_id` |
| `name` | profile `name`, defaulting to `public_id` |
| `owned_by` | `vekil-policy` |
| `supported_endpoints` | `[/chat/completions]` |
| `reasoning_effort` | lightweight route order, filtered to values also supported by powerful |
| `parallel_tool_calls` | true only when both terminal contracts support it |
| `vision` | always false in v1 |
| `context_window` | minimum positive value when both are known; otherwise omitted |
| `model_picker_enabled` | profile value, default true |
| `model_picker_category` | profile value, default `versatile` |

Both destinations must accept the same published Chat semantics. Per-target wire adaptations may differ only when they do not alter that public contract.

For a policy request, public JSON, SSE, safe model headers, errors, and client-facing metrics use the policy profile's public ID. This identity rule also applies when an unsupported request shape or Gemini surface is rejected locally before classification. Provider, terminal route, target, and deployment IDs do not leak through normalized policy output. Upstream `X-Request-ID` and `Request-ID` values are omitted; clients receive only the proxy-owned `X-Vekil-Request-ID` correlation header. Direct-route output behavior remains unchanged.

## Metrics and decision provenance

Policy telemetry must be attributable per profile and per declared request-size/tool-count traffic bucket. It reports:

- eligible, sampled, and admitted requests;
- deterministic sample exclusions and capacity drops;
- classifier completion/failure categories;
- selected/fallback decision distribution;
- classifier latency and usage/cost; and
- classifier-route breaker/health outcomes.

Observe analysis is not representative unless admission is at least 95% in every declared bucket or the missing population is evaluated separately. Observe data is supplementary operational evidence, not causal proof of quality, because all observed requests still execute the baseline tier.

Each bounded decision record carries IDs/enums/counts, latency/failure categories, and these generations:

- `configGeneration`: canonical normalized complete providers configuration;
- `profileGeneration`: normalized profile, derived public contract, and terminal route IDs;
- `classifierGeneration`: classifier route/target/model plus fact schema, forced-function schema, classifier-prompt, and mapper versions; and
- `binaryGeneration`: build version plus Git commit when available.

Generation hashes use normalized values and exclude secret values. Decision records, logs, and aggregate labels never contain prompt text, raw classifier output, tool arguments, credentials, or classifier rationale.

## Evaluation gates before enforcement

Do not enable production enforcement because observe-mode routing percentages look plausible. The operator release gate requires an actual end-to-end policy evaluation:

1. Maintain separate development, pilot/calibration, untouched holdout, and adversarial datasets.
2. Run at least 75 pilot tasks against always-lightweight, always-powerful, and the actual policy path.
3. Use pilot disagreement/variance for a documented power analysis.
4. Run the powered holdout with at least three independent executions per task/model/policy unless the analysis requires more.
5. Execute the real multi-turn policy path; do not splice outcomes from independent baseline trajectories.
6. Use deterministic tests/acceptance checks for objective coding tasks.
7. Use two blinded independent raters, or a predeclared external judge plus human disagreement audit, for subjective tasks.
8. Do not tune classifier choice, prompt, or mapping using holdout results.
9. Include classifier calls, terminal calls, retries, failures, and preflight amortization in cost.
10. Report observe sampling/admission bias separately.

Required statistical and safety gates are:

- at least 80% power at one-sided alpha `0.05`;
- task-success lower 95% confidence bound no worse than 2 percentage points below always-powerful;
- tool-call validity no worse than 0.5 percentage points below always-powerful;
- task-clustered confidence intervals for correlated tool calls/repeated runs;
- unavailable plus uncertain fallback rate at most 5% on both the predeclared holdout and live-observe denominator, excluding scheduled injected-chaos windows;
- mean total cost improvement at least 15%, including classifier cost;
- zero route, credential, cancellation, budget, or identity invariant failures;
- no extra terminal execution sends caused by policy selection;
- proxy overhead beyond synchronous classifier time at most 5% at p95;
- no measurable observe-mode p95 latency regression beyond fact construction; and
- adversarial tests cannot affect another request, profile capacity, or breaker state.

## Rollout and rollback

Keep the global ceiling `off` until the evaluation gates pass. Then:

1. Move selected profiles to `observe` independently.
2. Require at least 5,000 completed observations per profile and at least 95% admission in every declared traffic bucket.
3. Move one profile at a time to `enforce`.
4. When multiple replicas are available, canary stateless Chat traffic at 5% → 25% → 100% by deployment.
5. Keep direct stateful Responses/websocket traffic outside the policy canary pool or on its existing sticky topology.

Emergency rollback is process-wide:

```bash
POLICY_ROUTING_MODE=off vekil --providers-config /path/to/providers.yaml
```

V1 stores no policy affinity, so policy rollback requires no policy-session migration. Existing direct Responses/websocket state retains its separate process-local restart and affinity limitations.

## Deferred work

Future releases may add translated Gemini adapters, explicit cross-process Chat/Responses affinity, native Responses/websocket terminal selection, shared state, mixed Chat/Responses destinations, a local deterministic first stage, passive ranking, or a `versatile` middle tier.

Vekil continues to forbid arbitrary routing DSLs, recursive policies, post-output quality retries, cross-tier failure fallback, transparent provider-state migration, classifier-driven authorization, model-reported confidence as an enforcement input, content-derived shared breaker state, implicit prompt fingerprints as session identity, and public access to classifier routes.
