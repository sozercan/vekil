# Provider Routing and Authentication

Use this file when editing provider credentials, model ownership, JSON/YAML provider configs, provider header profiles, route exposure, or provider-specific model metadata. For schema-v2 classifier policy, privacy, and rollout details, see [Semantic Policy Routing](policy-routing.md). For where to obtain provider keys, see [Provider API Keys](provider-api-keys.md). For global flags and env vars, see [Configuration](configuration.md).

## Provider Authentication

### GitHub Copilot

For CI or other non-interactive environments, set `COPILOT_GITHUB_TOKEN` to a GitHub token for a user with GitHub Copilot access. This is the only GitHub token environment variable Vekil consumes directly; it overrides cached Vekil login state and is exchanged for a short-lived Copilot token at startup.

Vekil intentionally ignores generic GitHub token variables such as `GH_TOKEN` and `GITHUB_TOKEN`. If you want Vekil to use an authenticated GitHub CLI account, opt in explicitly with `vekil login --github-cli` or `vekil login --gh`; Vekil then runs `gh auth token --hostname github.com` for Copilot access and keeps that token in memory only, without copying it into Vekil's `access-token` or `api-key.json` caches.

Plain `vekil login` refreshes an existing Vekil-managed login when possible, otherwise starts GitHub's device-code flow. Use `vekil login --force` to force a new device-code flow even if an existing login can still refresh. A device-code sign-in disables GitHub CLI auto sign-in because the active account is then managed by Vekil rather than by `gh`.

After `vekil logout` or menubar Sign Out, Vekil clears its cached credentials, disables GitHub CLI auto sign-in, and suppresses automatic GitHub CLI reuse until you explicitly opt back in with `vekil login --github-cli` or `vekil login --gh`. `COPILOT_GITHUB_TOKEN` remains an explicit override and still works while signed out.

Concurrent Copilot-token refresh callers share one refresh attempt, but each caller still honors its own context deadline. The same rule applies to callers waiting for an interactive device flow.

### OpenAI Codex

OpenAI Codex uses the ChatGPT/Codex CLI credentials in `~/.codex/auth.json` by default. Set `CODEX_HOME` if your Codex home lives elsewhere.

OpenAI Codex requires file-based ChatGPT auth from `codex login`; API-key auth and OS keychain-backed credentials are not read by the proxy.

Concurrent refresh callers share one token refresh while retaining independent deadlines. On POSIX systems, Vekil persists rotated tokens back to the authoritative `auth.json` through a mode-`0600` `.vekil-cache` transaction journal: the journal is synced before the in-place auth update, removed only after the updated file is synced, and used to recover interrupted writes. Source-digest and inode checks keep an atomic external `codex login` replacement authoritative. On Windows, Vekil reads fresh `auth.json` credentials but does not perform token refresh; run `codex login` when the file becomes stale until equivalent secure journal and atomic-update semantics are available.

### Azure OpenAI and Microsoft Foundry

Azure providers support two auth modes:

- **API key auth**: omit `auth_mode` or set `auth_mode: api_key`, then configure either `api_key` or `api_key_env`. This preserves the existing behavior and sends Azure's `api-key` header upstream.
- **Microsoft Entra auth**: set `auth_mode: azure_identity`. Vekil uses the Azure SDK `DefaultAzureCredential` chain, sends `Authorization: Bearer <token>`, and does not send an `api-key` header. Do not configure `api_key` or `api_key_env` in this mode.

For Entra auth, `token_scope` is optional and defaults to `https://ai.azure.com/.default`, which is appropriate for Microsoft Foundry OpenAI-compatible endpoints. Override it if your resource requires a different Azure audience, such as `https://cognitiveservices.azure.com/.default` for classic Azure OpenAI deployments.

Vekil does not run `az login` for you. For local development, sign in with Azure CLI or another credential supported by `DefaultAzureCredential`; in hosted environments, use managed identity, workload identity, or environment credentials. The signed-in principal needs the required Azure RBAC role, for example Cognitive Services OpenAI User on the target resource.

Entra token refreshes are shared across concurrent requests; waiters can time out or cancel without waiting for the leader refresh to finish.

### Generic Providers

`openai-compatible` and `anthropic-compatible` providers use the generic auth fields:

- `auth_type: bearer` sends `Authorization: Bearer <key>` by default.
- `auth_type: api-key-header` sends the key through `auth_header`, with optional `auth_prefix`.
- `auth_type: none` sends no auth header, useful for local providers.
- `extra_headers` adds fixed provider headers after client Copilot-identifying headers are stripped.

When `auth_type` is omitted, Vekil uses `bearer` if `api_key` or `api_key_env` is set, otherwise `none`. If `api_key_env` is configured, the referenced environment variable must be set and non-empty at startup.

## Native macOS Guided Editor Scope

The native source adds the Go-owned managed draft/validation/apply boundary and Swift Keychain/form-state foundations without changing the provider schema or routing engine. The current Providers view deliberately keeps the full guided editor and **Validate and Apply** action disabled until the signed cross-version Keychain gate is complete. Go remains responsible for typed draft conversion, deterministic YAML, strict validation, public-model ownership, endpoint policy, and structured field errors.

The guided surface is intended for Copilot, OpenAI Codex, Azure OpenAI API-key/Entra modes, and supported generic OpenAI-compatible and Anthropic-compatible auth/discovery modes. It does **not** edit explicit `model_routes`, ordered failover, `policy_profiles`, custom headers, path overrides, trust metadata, or tool optimizers. Those remain file-owned advanced configuration. Unsupported or out-of-band managed fields must produce drift rather than being silently deleted; use External Configuration for advanced routing.

Public model IDs remain global across providers, and endpoint fields remain declared native allowlists. A UI selection is not evidence of upstream compatibility unless validation or a real probe ran. Dynamic providers still need safe model allowlists when required to avoid ownership collisions.

Managed keys are resolved through the implemented provider-scoped secret resolver from an immutable helper projection rather than `os.Environ`; External Configuration retains the existing auth behavior documented here. The managed transaction and Keychain services are implemented, but the guided editor is not release-enabled. A signed A-to-B Sparkle continuity test is mandatory before Keychain-backed managed providers can ship. See [Provider API Keys](provider-api-keys.md#macos-managed-provider-secrets).

## Provider Routing

Use `--providers-config` when you want explicit ownership of public model IDs across providers such as GitHub Copilot, Azure OpenAI, OpenAI Codex, or generic OpenAI-compatible and Anthropic-compatible upstreams. Provider config files can be JSON (`.json`) or YAML (`.yaml`/`.yml`).

Provider config decoding is strict. Unknown fields and duplicate JSON/YAML mapping keys are rejected so typos or ambiguous values do not silently change routing. A JSON file must contain exactly one value, and a YAML file must contain exactly one document. Schema-version-2 YAML also rejects merge keys (`<<`); expand anchors into explicit fields before migrating a version-1 file.

You can run Azure-only or Codex-only configs, or mix those providers with Copilot behind the same local endpoint.

### Configuration versions

A provider file with no `schema_version` is version 1. Explicitly setting `schema_version: 0` is invalid. Only schema versions 1 and 2 are supported. Version-1 files keep the existing provider-owned `models[]`, dynamic discovery, default-provider, unknown-model, catalog, and retry behavior. A new binary does not rewrite those files.

Schema version 2 is the complete explicit-routing schema. It adds public and internal `model_routes`, ordered target failover, provider trust metadata, internal classifier routes, and optional semantic `policy_profiles`. A route-only version-2 file remains valid without policy fields. A sole provider is the implicit default, and in a multi-provider configuration a single Copilot provider is the implicit default. Multiple non-Copilot providers may omit `default: true` only when none exposes a legacy static or dynamic model catalog and client-visible models are owned by public routes or policies. If any such provider exposes legacy/catalog models, configure exactly one explicit default provider. A complete route-only environment-variable example is checked in at [`examples/provider-routing-failover.yaml`](../examples/provider-routing-failover.yaml).

```yaml
schema_version: 2

providers:
  - id: azure-primary
    type: azure-openai
    default: true
    base_url: https://primary-resource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_PRIMARY_API_KEY

  - id: azure-secondary
    type: azure-openai
    base_url: https://secondary-resource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_SECONDARY_API_KEY

model_routes:
  - id: gpt-5-4-pro-route
    public_id: gpt-5.4-pro
    name: GPT-5.4 Pro
    endpoints:
      - /responses
    reasoning_effort:
      - low
      - medium
      - high
    parallel_tool_calls: true
    vision: false
    context_window: 200000
    model_picker_enabled: true
    model_picker_category: versatile

    targets:
      - id: primary
        provider: azure-primary
        upstream_model: gpt-5.4-pro
      - id: secondary
        provider: azure-secondary
        upstream_model: gpt-5.4-pro

    routing:
      mode: primary_only
      max_target_attempts: 1
      max_upstream_sends: 1
```

After the route has been validated in `primary_only`, enable ordered failover explicitly:

```yaml
routing:
  mode: priority_failover
  max_target_attempts: 2
  max_upstream_sends: 2
```

The two targets must implement the same public contract. If endpoint support, reasoning behavior, tool semantics, vision support, context limits, or other client-visible behavior differs, expose separate public routes instead of putting the targets in one pool. Target order is failover order; schema version 2 has no weights, random picker, affinity, or sticky-routing field.

The same schema-version-2 contract also includes:

- `model_routes[].exposure: public|internal`, with omitted exposure defaulting to `public`;
- internal-purpose classifier routes;
- provider `trust_domain` and classifier non-storage capability metadata; and
- top-level `policy_profiles` that publish one Chat model ID and select a `lightweight` or `powerful` canonical-Chat terminal route backed by native Chat or bounded Chat-over-Responses.

These fields are additive: existing route-only version-2 configurations remain valid, while version-1 files reject explicit-route and policy-routing fields. Internal routes have no public ID, aliases, picker metadata, `/v1/models` entry, dashboard insight-model eligibility, or direct client resolution. Policy destinations may be public or internal; exposed public destinations are deliberate policy bypasses. The recommended policy configuration keeps both destinations and the classifier internal. See [`examples/policy-routing-coding-economy.yaml`](../examples/policy-routing-coding-economy.yaml), the single-process [`examples/policy-routing-copilot.yaml`](../examples/policy-routing-copilot.yaml), and [Semantic Policy Routing](policy-routing.md).

### Route schema and validation

For a public version-2 route, required route fields are `id`, `public_id`, an explicit nonempty `endpoints` allowlist of verified native upstream operations, and at least one target. A version-2 internal route requires `id`, `exposure: internal`, endpoints, and targets but must omit `public_id` and picker metadata. Each target requires a route-local `id`, `provider`, and `upstream_model`. Provider, route, and target operational IDs in schema version 2 are limited to 128 bytes and restricted to bounded ASCII identifier characters. The initial implementation accepts at most 256 routes, 32 targets per route, and 1,024 explicit targets total.

Public IDs are globally unique across explicit routes, policy profiles, static provider models, and dynamically discovered models, including supported normalized aliases. A public ID cannot be declared in more than one of `providers[].models[]`, public `model_routes[]`, or `policy_profiles[]`. Explicit public entries reserve their IDs against later discovery; a dynamic refresh collision rejects that whole refresh and retains the last-known-good registry. Duplicate route IDs, target IDs, endpoints, or reasoning efforts are errors rather than silently deduplicated values. Route-only static Azure or generic providers may omit `models`; unreferenced static providers still need their normal model declarations. Dynamic generic providers keep their existing omission and discovery rules. Copilot is the explicit catalog-driven exception: any explicit route may pin an `upstream_model` from its authenticated dynamic catalog without exposing that physical target as a separate legacy model. Provider filters are enforced during config validation, and startup discovery requires every in-scope pinned model to remain in the canonical Copilot catalog with each declared endpoint before readiness. Pinned Copilot target IDs are suppressed from the provider's legacy public catalog and direct unknown-model routing; configured public routes and policy profiles remain routable through their registered public IDs. Classifier preflight then verifies the forced-tool protocol.

Catalog metadata belongs to the route, not to whichever target answered last. Explicit Responses output, OpenAI Chat JSON/SSE, websocket metadata, and translated Anthropic/Gemini output normalize to the public route ID. Legacy raw OpenAI Chat routes retain their existing conservative model-field behavior. The configurable route metadata is `name`, `endpoints`, `reasoning_effort`, `parallel_tool_calls`, `vision`, `context_window`, `model_picker_enabled`, and `model_picker_category`. Omitted values use deterministic static-catalog defaults: `name` equals `public_id`, picker enabled, category `versatile`, false booleans, and no published numeric limit. Temporary target failure never removes or changes the catalog entry.

Request-policy fields are intentionally narrow. `model_routes[].reasoning_effort` is a route capability allowlist and, for a direct public route, catalog metadata; it does not inject a value. There is no route-level reasoning default.

Policy profiles may instead opt into policy-owned reasoning by setting `reasoning_effort` in both required tier objects. Omitting effort from both tiers preserves policy execution with no injected value and rejects a valid present client effort as unsupported. When configured, each tier value must appear in its referenced terminal route's allowlist, and the selected value replaces incoming OpenAI Chat, Anthropic, or Responses effort before native-Chat or Chat-over-Responses execution. The sealed value is rebuilt identically for every target attempt, so failover inside the selected route cannot change it. Policy public catalog metadata never advertises client-selectable reasoning effort, whether or not tier effort is configured.

For example, terminal routes declare capability while the profile owns the selected values:

```yaml
model_routes:
  - id: lightweight-route
    endpoints: [/chat/completions]
    reasoning_effort: [low, medium]
    # targets: ...
  - id: powerful-route
    endpoints: [/responses]
    reasoning_effort: [high, max]
    # targets: ...

policy_profiles:
  - id: semantic-policy
    lightweight:
      route: lightweight-route
      reasoning_effort: low
    powerful:
      route: powerful-route
      reasoning_effort: max
    # classifier/data_policy: ...
```

Route-level `drop_sampling_params` and `drop_stop_sequences` likewise apply uniformly to every target. `drop_stop_sequences` removes OpenAI Chat `stop` after protocol translation; this is an explicit semantics relaxation for upstream models that reject the field, including Claude Code side queries that originate as Anthropic `stop_sequences`. It is valid only for provider models and routes whose endpoint set includes `/chat/completions`; Responses-only declarations are rejected during configuration validation. Target-level `use_max_completion_tokens` is available only as a semantics-preserving Chat Completions wire rewrite (`max_tokens` to `max_completion_tokens`). Do not use per-target fields to create different public behavior inside one route.

The binary validates a compiled provider/native-endpoint/surface/mode feature matrix. A provider kind, native endpoint, public translation surface, or routing mode that the running binary does not implement is rejected during validation rather than accepted with degraded behavior. Native route endpoints remain `/responses`, `/chat/completions`, and `/v1/messages`. OpenAI Chat plus translated Anthropic and Gemini requests enter canonical Chat execution, which prefers a route's `/chat/completions` endpoint and otherwise uses `/responses`; direct `anthropic-compatible` Messages uses `/v1/messages`. See [Supported route surfaces](#supported-route-surfaces) below.

Validate a file without serving or modifying it:

```bash
vekil config validate --providers-config /path/to/providers.yaml
```

The command performs strict JSON/YAML decoding, provider/target reference checks, collision and limit checks, adapter compatibility checks, route-budget validation, and catalog compilation. It does not start the HTTP server or contact model/inference endpoints. Local auth configuration must still be usable: for example, a referenced `api_key_env` must be populated, and local credential construction may fail validation before any network request.

For schema-v2 policies, explicitly request classifier protocol preflight with:

```bash
vekil config validate --live --providers-config /path/to/providers.yaml
```

`--live` sends one fixed non-user fixture per distinct classifier route selected by the policy config to verify authentication/reachability, forced strict function output, configured non-storage behavior, and the one-send contract. It does not prove the provider's retention policy. Configuration reload is not part of schema version 2; apply changes by restarting Vekil.

### Routing modes and budgets

- `primary_only` always selects the first eligible configured target, performs no automatic target switch, and never skips the primary because of cached health. It is the safe default and immediate per-route rollback.
- `priority_failover` considers unattempted targets in configuration order, but only while the replay-safety gate remains open. It does not balance healthy requests across targets.
- Changing only `mode` does not increase the budgets: with omitted budget fields, `priority_failover` still has one target attempt and one send. Configure larger values explicitly to permit a secondary.
- `max_target_attempts` defaults to `1` when omitted, includes the first target, and cannot exceed the configured target count. An explicitly configured `0` is invalid, and `primary_only` requires this value to remain `1`.
- `max_upstream_sends` defaults to `1` when omitted, must be at least `max_target_attempts`, and caps physical inference POSTs for one logical operation. An explicitly configured `0` is invalid. A named same-target protocol recovery, compact/replay child call, or compatibility-model call also consumes a send when that path is integrated with explicit routes; it is not a free retry or another target attempt.
- Explicit routes do not nest the broad legacy same-target transport retry loop inside each target. Version-1 and compiled legacy routes retain their existing retry behavior, and the existing `retries` metric continues to describe those same-target retries.
- Size `max_upstream_sends` for every reachable child send, not just normal targets. Responses compaction/replay, encrypted-content cleanup, stream-options recovery, and compatibility-model recovery all draw from the same route operation; the route cap can stop them before their own local fanout/recovery limit.

One total inference deadline is shared across all attempts. It is not restarted per target, and the initial behavior does not allocate a smaller timeout to a hanging primary. Redirects and implicit transport body replay are disabled for inference sends so the send budget matches actual dispatches and credentials cannot follow redirects.

Automatic target switching is intentionally narrow:

| Observed outcome | Switch to the next target |
|------------------|---------------------------|
| DNS, dial, or TLS failure before request bytes could be written | Yes, if admission, deadline, cleanup, and budgets still allow it |
| Authoritative HTTP `429` | Yes |
| Adapter-certified pre-execution overload/unavailable rejection, such as a supported `503`/`529` | Yes |
| Adapter-certified pre-output Responses terminal admission failure | Yes, only when that exact condition proves no semantic/tool execution |
| Client cancellation, shutdown, or total operation deadline | No |
| Authentication, configuration, invalid-request, or content-policy error | No |
| Reset/timeout after request write, generic `502`/`504`, or other ambiguous delivery | No |
| Partial success body, text/reasoning/tool output, malformed/unknown event, or any downstream commitment | No |
| Known provider state owned by the previous target, or unknown/conflicting explicit-route state | No |

Attempts never overlap. Before switching, the failed response body and local readers/pumps must terminate. If delivery, semantic progress, commitment, state ownership, or cleanup is uncertain, Vekil returns an error instead of risking a duplicate generation, duplicate billing, duplicate server-side tool activity, or corrupted continuation.

When several attempts fail, error selection is deterministic: ambiguous delivery wins over local state/configuration/authentication errors, which win over authoritative retryable rejections, which win over no-eligible-target exhaustion. If every attempt was an authoritative retryable rejection, the first attempted target's canonical protocol error is preserved; failed-attempt headers are not merged into a later response.

### Supported route surfaces

The current explicit-target matrix is static except for explicitly pinned
Copilot targets:

| Native route endpoint | Allowed explicit target providers | Public surfaces using the operation | `priority_failover` boundary |
|--------------------------|-----------------------------------|-------------------------------------|------------------------------|
| `/responses` | `copilot`; `azure-openai`; static `openai-compatible` | Direct `POST /v1/responses`; route-aware compact/memory/replay helpers; optional proxy-owned `GET /v1/responses` websocket bridge; OpenAI Chat, translated Anthropic/Gemini, token probes, and dashboard insights through Chat-over-Responses when the request subset permits | Prewrite or adapter-certified pre-execution rejection; direct Responses and Responses-backed Chat streams may also switch after a held Responses preamble and a certified non-executing terminal admission failure, but never after semantic/tool progress or downstream commitment |
| `/chat/completions` | `copilot`; `azure-openai`; static `openai-compatible` | OpenAI Chat Completions; translated Anthropic Messages; Gemini `generateContent` / `streamGenerateContent`; Chat-based token probes and dashboard insights | Prewrite or adapter-certified `429`/overload rejection; client streams can switch only while the held prefix is nonsemantic, and forced-stream aggregation only before text/reasoning/tool progress |
| `/v1/messages` | static `anthropic-compatible` | Direct native Anthropic Messages | Prewrite or adapter-certified `429`/overload rejection while only a nonsemantic Anthropic preamble is held; no mixing with OpenAI-translated targets |

Native Anthropic `POST /v1/messages/count_tokens` is a bounded compatibility operation in the selected public route. In `priority_failover` mode it may switch targets only under the same replay-safe, precommit, adapter-certified rejection rules and shared target/send budgets as other route operations; protocol-recovery child sends remain pinned to their selected target. Chat-based Anthropic/Gemini token probes use canonical Chat execution and therefore the native `/chat/completions` or `/responses` endpoint selected from the route allowlist.

For explicit client streams, Vekil bounds precommit inspection with the existing `750 ms` timeout and `64 KiB` prefix limit. Reaching either bound commits the current target and forwards the buffered prefix; unknown/malformed events, semantic output, tool activity, usage/accounting frames, or a client write also prohibit switching.

An OpenAI-family route may use Copilot, Azure, or static OpenAI-compatible targets when all targets implement every advertised endpoint with equivalent semantics. Copilot targets use the configured `upstream_model` directly and authenticate through the process's normal Copilot authenticator; they do not require a second loopback Vekil bridge. An Anthropic-family route contains only static Anthropic-compatible targets. OpenAI Codex, dynamically discovered generic providers, native `/realtime`, and heterogeneous OpenAI/native-Anthropic target sets are rejected as explicit route targets. Provider-only version-1 routing for those providers remains available.

Schema-v2 policy selection is narrower than this general explicit-route matrix. Both terminal routes and the classifier route must support canonical Chat execution through either native `/chat/completions` or Vekil's bounded Chat-over-Responses adapter. Copilot-backed Responses routes authenticate and adapt in process, so `vekil launch` remains a single command. The policy public ID still advertises `/chat/completions` and accepts text/function-tool OpenAI Chat, translated Anthropic Messages/counting, and bounded stateless Responses compatibility. It is rejected on the Responses websocket, compact/memory routes, hosted/custom tools, Gemini, multimodal input, and stateful `previous_response_id`. Process-local `call_vekil_*` continuations remain bound to their originating terminal route/tier; opaque downstream-bridge replay still requires the documented single-target `off`/`observe` baseline and sticky ingress. Direct public routes keep the general matrix above.

The optional websocket bridge is still transport adaptation over HTTP `/responses`. Its first provider-backed `response.create` may use the same safe precommit route failover; after a successful target is exposed, the session is pinned to that exact route/target and later turns fail closed rather than migrate. See [Responses WebSocket Bridge](responses-websocket.md).

### Exact state binding and process-local limits

Provider-issued state is bound to one exact `{route_id, target_id}`. This includes adapter-marked response IDs, trusted `X-Codex-Turn-State`, non-proxy opaque `encrypted_content`, and other opaque reasoning/session handles. Known state pins the owning target and disables failover. All supplied state values must agree; malformed, conflicting, cross-route, or mixed known/unknown state on an explicit `/responses` operation fails locally without an upstream call. A token observed from different owners becomes a conflict tombstone and remains fail-closed until that record expires or is evicted.

There is one narrow first-use exception for a client-supplied Responses `conversation` ID. When that conversation is the request's only explicit state and the route can select exactly one eligible Responses target, Vekil atomically binds the ID to that target before dispatch and hard-pins the operation. This covers a one-target route and a multi-target `primary_only` route whose configured primary is eligible. An unknown conversation on a multi-target `priority_failover` route remains fail-closed because ownership is ambiguous; other unknown provider state also remains fail-closed. `previous_response_id` cannot be combined with `conversation`. Vekil exposes no public conversation-registration endpoint and accepts no client target hint.

The binding index is bounded to 262,144 entries with a 24-hour absolute TTL and is process-local. Capacity eviction, expiry, restart, or sending the next request to another Vekil process makes a prior binding unknown. For ordinary provider state that fails closed. A conversation-only request on a currently deterministic route can instead take the bootstrap path, which cannot distinguish genuine first use from a lost prior binding; keep the process affinity and deterministic target stable for the lifetime of active conversations. Lookups update recency for eviction but do not extend the absolute TTL; observing the same token again from the same owner refreshes it. **Every explicit Responses route that accepts provider-issued state requires one Vekil process or sticky ingress to the process that owns the binding**, including one-target and `primary_only` routes. Responses-backed Chat tool continuations use a separate process-local replay store and have the same affinity/restart constraint. Vekil does not migrate Responses state, replay a WebSocket session onto another target, or infer portability from user-provided strings. Durable/shared bindings and proxy-signed target hints are future work.

### No terminal-route balancing or circuit breaker

Schema version 2 deliberately does not include active-active/weighted routing, automatic affinity extraction, bounded-load selection, active health probes, user-defined quota/cache/failure domains, half-open terminal-target circuit-breaker state, configured cross-model fallback, or cross-route fallback. Temporary target errors also do not change `/readyz`. Any future exact-target cooldown based on authoritative `Retry-After` data requires separate implementation; no generic circuit-breaker framework is implied by `priority_failover`.

Schema-v2 policy routing adds a separate infrastructure-only breaker for the **classifier route**, not for terminal target selection. Only pre-inference transport failures, `429`, and upstream `5xx` affect it. Timeouts, malformed classifier output, missing forced calls, abstention, content-dependent latency, and user validation errors do not change shared health. A selected terminal route still follows only its own configured `primary_only` or replay-safe `priority_failover` behavior; classifier failure selects the profile's configured unavailable tier and never creates cross-tier failover.

### Rollout and rollback

A conservative rollout is: validate the version-2 file, run a one-target route in `primary_only`, add the secondary while still `primary_only`, verify catalog identity and attempt metrics, then enable `priority_failover` with explicit budgets for one canary route. Inject only replay-safe failures such as a primary `429` or prewrite dial failure when testing the switch.

Policy profiles have a separate operator gate: keep the global ceiling `off`, complete the powered end-to-end evaluation, graduate profiles independently through `observe`, require at least 5,000 completed observations and 95% admission in every declared traffic bucket, then enforce one profile at a time. See [Policy evaluation gates](policy-routing.md#evaluation-gates-before-enforcement) and [Policy rollout and rollback](policy-routing.md#rollout-and-rollback).

To stop automatic switching, restore `mode: primary_only` and restart the same schema-version-2 binary. This is availability-safe but **not continuity-preserving**: restart clears process-local state bindings, Responses-backed Chat replay state, and WebSocket sessions, so drain stateful traffic first or accept deterministic continuation failures. The new process still interprets newly issued state with the same version-2 fail-closed rules. Do **not** restore a version-1 file or downgrade to an older binary while state issued by a secondary may still be presented. First fence new stateful continuations, drain WebSocket sessions, wait at least the 24-hour binding TTL plus any longer provider replay window, and perform an atomic no-mixed-version cutover. If that fence cannot be guaranteed, keep the running version-2 process in `primary_only` until the fence is complete. There is no automatic configuration migration in either direction.

### Native endpoints and Chat compatibility

For canonical Chat requests, Vekil first prefers native `/chat/completions` when both provider policy and the selected model or explicit-route allowlist permit it. If native Chat is unavailable but native `/responses` is allowed, Vekil executes the request through its Chat-over-Responses adapter. This makes these public compatibility surfaces available to a Responses-native model without changing provider ownership:

- `POST /v1/chat/completions`;
- translated `POST /v1/messages` and `/v1/messages/count_tokens`;
- Gemini `generateContent`, `streamGenerateContent`, and `countTokens`;
- the dashboard insight call.

This is served compatibility, not native capability metadata. Keep `models[].endpoints` and `model_routes[].endpoints` limited to verified native upstream routes. A Responses-only model or explicit route must remain configured with only `/responses`, and `/v1/models` continues to render only that native endpoint even though Vekil can translate Chat-compatible traffic to it. Do not add `/chat/completions` merely to advertise the adapter. See the [Responses-backed Chat request subset](api.md#responses-backed-chat-request-subset) for the strict translation boundary.

On an unknown model routed to an unfiltered dynamic provider, the first Chat-compatible request may perform one provider-local model refresh before choosing a backend. Discovery is coalesced per provider, bounded to two seconds, cached for five minutes after success, and backed off for five seconds after failure. It does not require or populate the merged public `/v1/models` cache.

### Azure-Only Example

```yaml
providers:
  - id: azure-openai
    type: azure-openai
    default: true
    base_url: https://myresource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
```

### Microsoft Foundry Entra Example

```yaml
providers:
  - id: foundry
    type: azure-openai
    default: true
    auth_mode: azure_identity
    # Optional; defaults to https://ai.azure.com/.default
    token_scope: https://ai.azure.com/.default
    base_url: https://myresource.services.ai.azure.com/api/projects/myproject/openai/v1
    models:
      - public_id: gpt-5.4
        deployment: gpt-5.4
        endpoints:
          - /responses
        name: GPT-5.4
```

### Copilot + Azure Example

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    exclude_models:
      - gpt-5.4-pro
  - id: azure-openai
    type: azure-openai
    base_url: https://myresource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
```

### OpenAI Codex Subscription Example

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
  - id: openai-codex
    type: openai-codex
    include_models:
      - gpt-5.5
```

JSON configs use the same snake_case field names as YAML.

### Generic Provider Behavior

OpenAI-compatible providers use `chat_completions_path` for models with native `/chat/completions` support. If a model is configured as `/responses`-only, OpenAI Chat plus translated Anthropic and Gemini traffic is converted and sent to `responses_path` instead. `POST /v1/responses` support itself is never inferred; add `/responses` to a model only after validating the upstream model and path.

Anthropic-compatible providers directly forward Anthropic `POST /v1/messages` to `messages_path`. They do not serve OpenAI Chat Completions or Responses routes.

`model_discovery` can be `static`, `openai`, `ollama`, or `openrouter-tools`. OpenAI discovery reads an OpenAI-style `data` array. Ollama discovery reads `/api/tags`. OpenRouter-tools discovery reads an OpenAI/OpenRouter-style `data` array and exposes only models that advertise tool parameters.

Successful decoded dynamic model catalogs are capped at 4 MiB before JSON decoding. Oversized Copilot, Codex, or generic catalogs fail without updating routing/cache state; an oversized optional Azure metadata overlay falls back to the configured static catalog. Concurrent requests for the same `/v1/models` query variant share one upstream refresh, and failed canonical refreshes briefly back off for one second while stale canonical data remains available.

### Generic Provider Field Reference

| Field | Applies To | Purpose |
|-------|------------|---------|
| `type` | all providers | Use `openai-compatible` or `anthropic-compatible` for generic providers. |
| `base_url` | generic providers | Upstream origin and any fixed API prefix. The proxy appends only the configured path field. |
| `api_key`, `api_key_env` | generic providers | Static credential value or the name of any environment variable you choose. |
| `auth_type` | generic providers | `bearer`, `api-key-header`, or `none`. Defaults to `bearer` when a key is present, otherwise `none`. |
| `auth_header`, `auth_prefix` | generic providers | Header name and optional prefix for `api-key-header`, or overrides for bearer auth. |
| `extra_headers` | generic providers | Fixed headers to add to every upstream request after client Copilot headers are stripped. |
| `chat_completions_path` | `openai-compatible` | Upstream native Chat path, used when the selected model allows `/chat/completions`. Defaults to `/chat/completions`. |
| `responses_path` | `openai-compatible` | Upstream native Responses path for direct Responses and Responses-backed Chat. Defaults to `/responses`; models must still opt in with `/responses`. |
| `messages_path` | `anthropic-compatible` | Upstream path for public `POST /v1/messages`. Defaults to `/v1/messages`. |
| `models_path` | generic providers | Upstream path for dynamic model discovery and readiness probes. Defaults to `/models`. |
| `model_discovery` | generic providers | `static`, `openai`, `ollama`, or `openrouter-tools`. |
| `trust_domain` | providers used by schema-v2 policy destinations/classifiers | Operator-defined data-governance domain. Required for every provider referenced by a policy; matching is enforced unless the profile acknowledges cross-domain forwarding. |
| `classifier_no_store_supported` | provider used by a schema-v2 classifier route | Declares that Vekil can send the provider's supported non-storage option for classifier requests. This is a capability declaration, not proof of retention behavior. |
| `models[].endpoints` | all static models | Verified native upstream endpoint allowlist. It controls rendered capability metadata and native backend selection; Vekil does not add served compatibility routes. |
| `models[].drop_stop_sequences` | static models with `/chat/completions` | When `true`, remove OpenAI Chat `stop` after translation, including translated Anthropic `stop_sequences`. This changes stop behavior; use only when the upstream rejects the field and the client can tolerate model-determined termination. |
| `models[].use_max_completion_tokens` | static models with `/chat/completions` | When `true`, rewrite translated Chat/Anthropic `max_tokens` to `max_completion_tokens` before forwarding. Use only for deployments that reject the legacy field. |

### Generic Provider Cookbook

Chat-only OpenAI-compatible hosted providers, including NVIDIA NIM and Kimi, should expose only `/chat/completions` unless you have validated `/responses` for that exact model:

```yaml
providers:
  - id: hosted-chat
    type: openai-compatible
    default: true
    base_url: https://provider.example.com/v1
    api_key_env: PROVIDER_API_KEY
    models:
      - public_id: provider-chat-model
        deployment: upstream-chat-model
        endpoints:
          - /chat/completions
```

LM Studio, llama.cpp, and AIKit usually fit the same shape with local auth disabled:

```yaml
providers:
  - id: local-openai
    type: openai-compatible
    default: true
    base_url: http://localhost:1234/v1
    auth_type: none
    models:
      - public_id: local-chat
        deployment: local-model
        endpoints:
          - /chat/completions
```

For AIKit's default quick-start port, use `base_url: http://localhost:8080/v1` and set `deployment` to the model name served by the image, such as `llama-3.1-8b-instruct`.

Z.ai-style OpenAI-compatible providers can use the same config, but set `base_url` exactly to the upstream API base documented by the provider. Do not append `/v1` unless the provider's OpenAI-compatible base URL includes it.

Ollama can use `/api/tags` discovery and OpenAI-compatible chat routing:

```yaml
providers:
  - id: ollama
    type: openai-compatible
    default: true
    base_url: http://localhost:11434
    auth_type: none
    model_discovery: ollama
    models_path: /api/tags
    chat_completions_path: /v1/chat/completions
```

OpenAI-compatible providers with validated Responses support, including OpenCode Zen/Go models documented for Responses, should opt in per model:

```yaml
providers:
  - id: responses-provider
    type: openai-compatible
    default: true
    base_url: https://provider.example.com/v1
    api_key_env: PROVIDER_API_KEY
    models:
      - public_id: responses-model
        deployment: upstream-responses-model
        endpoints:
          - /responses
```

#### OpenCode Zen Free Tier

OpenCode Zen is an OpenAI-compatible gateway at `https://opencode.ai/zen/v1`. Its free models can be reached anonymously with the literal sentinel key `public` (the same value the opencode client sends when no real key is configured). No signup, OAuth, or token refresh is involved, so this maps directly onto an `openai-compatible` provider with `auth_type: bearer` and `api_key: public`. A ready-to-run config is in [`examples/opencode-zen-free.yaml`](../examples/opencode-zen-free.yaml):

```yaml
providers:
  - id: opencode-zen
    type: openai-compatible
    base_url: https://opencode.ai/zen/v1
    auth_type: bearer
    api_key: public                  # shared anonymous sentinel, not a secret
    model_discovery: static
    models:
      - public_id: deepseek-v4-flash-free
        endpoints:
          - /chat/completions
      - public_id: big-pickle
        endpoints:
          - /chat/completions
```

Operational notes for the free tier:

- Use `model_discovery: static`, not `openai`. Dynamic discovery lists the full Zen catalog, including paid models that reject the `public` key with `401`, and there is no cost-based filter (only `include_models`/`exclude_models`). Static discovery also skips the upstream readiness probe.
- Do not set `default: true`. Under static discovery, unlisted models return `400`, so making this the catch-all only risks routing unknown models to a revocable trial gateway.
- The free set rotates and individual promotions end without notice. When a promo ends, that model returns an error body (observed as `401`) such as `Free promotion has ended for <model>`. Re-check the live set before relying on it: `curl -s https://opencode.ai/zen/v1/models -H 'authorization: Bearer public'`.
- `public` is a shared anonymous credential, rate-limited server-side per IP. It suits personal, low-volume use, not fan-out or automation.
- Free models are not zero-retention ("collected data may be used to improve the model"). `north-mini-code-free` (Cohere) and `nemotron-3-ultra-free` (NVIDIA) add "trial use only / do not submit confidential data" terms. Do not route proprietary or sensitive prompts through them.
- `/responses` support is per model. It is verified working for `deepseek-v4-flash-free`, `north-mini-code-free`, and `nemotron-3-ultra-free`; `big-pickle` returns `401` ("not supported for format openai"). Add `/responses` to a model's `endpoints` only after confirming it, and keep the others `/chat/completions`-only.
- Client compatibility: Claude Code (`/v1/messages`) and Gemini CLI translate to `/chat/completions` and work against the free tier. The GitHub Copilot CLI works in offline BYOK mode with `COPILOT_PROVIDER_WIRE_API=completions`. The OpenAI Codex CLI does not work against Zen free models: it is `/responses`-only and always sends a built-in `web_search` tool with no `name`, which the free upstreams reject during responses→chat translation.

For the full (paid) Zen catalog or higher limits, sign in at [opencode.ai/auth](https://opencode.ai/auth) and swap `api_key: public` for `api_key_env: OPENCODE_API_KEY` (still a static key, still no refresh). Swapping the key alone is not enough to route paid models: under `model_discovery: static` only the listed `models:` entries are routable, so add each paid model's `public_id` (and `endpoints`) you want to use, or switch to `model_discovery: openai` with `include_models` to opt into specific discovered paid IDs. Validate the free tier end to end with [`scripts/live-zen-smoke.sh`](../scripts/live-zen-smoke.sh) (a quick `curl`/`jq` check), or with the CLI-driven [`Live OpenCode Zen Smoke`](../.github/workflows/live-zen-smoke.yaml) workflow described in [Development](development.md#live-opencode-zen-cli-smoke-workflow).

Anthropic-compatible providers with native Messages support, including Wafer, OpenRouter, and DeepSeek-style Messages endpoints, should not advertise OpenAI routes:

```yaml
providers:
  - id: native-messages
    type: anthropic-compatible
    default: true
    base_url: https://provider.example.com
    api_key_env: PROVIDER_API_KEY
    auth_type: api-key-header
    auth_header: x-api-key
    messages_path: /v1/messages
    models:
      - public_id: claude-compatible
        deployment: upstream-messages-model
        endpoints:
          - /v1/messages
```

If LM Studio, llama.cpp, or Ollama exposes a native Anthropic Messages endpoint in your local setup, configure it as a separate `anthropic-compatible` provider with `messages_path`. Do not rely on `openai-compatible` to direct-forward Messages; that type translates Anthropic requests through Chat Completions.

Dynamic Copilot catalog entries are also eligible for native Anthropic Messages forwarding when their `supported_endpoints` explicitly includes `/v1/messages`. Vekil performs bounded catalog discovery before routing the first Messages request for an unknown Copilot model, then forwards the original Anthropic request directly so fields such as `thinking` and `stop_sequences` retain their native semantics. Chat-only, endpoint-less, and still-unknown Copilot models continue through Chat translation instead of assuming native Messages support.

### Copilot Provider Header Profiles

`type: copilot` providers can define a `headers` block with a provider-wide `default` profile and endpoint-specific `chat_completions` and `responses` profiles. Endpoint-specific values override `headers.default`, which overrides the global Copilot header flags/environment variables, which then fall back to the built-in defaults. Omitted fields inherit from the lower-precedence profile. The built-in `openai-intent: conversation-panel` fallback is endpoint-aware: it is applied to upstream `/chat/completions` and `/responses` calls, while upstream `/models` calls send `openai-intent` only when you configure it explicitly through `--copilot-openai-intent`, `COPILOT_OPENAI_INTENT`, or a provider header profile.

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    headers:
      default:
        editor_version: vscode/1.95.0
        editor_plugin_version: copilot-chat/0.26.7
        user_agent: GitHubCopilotChat/0.26.7
        copilot_integration_id: vscode-chat
        github_api_version: "2025-05-01"
      chat_completions:
        openai_intent: conversation-panel
      responses:
        openai_intent: agent-mode
```

Supported header fields are `editor_version`, `editor_plugin_version`, `user_agent`, `copilot_integration_id`, `github_api_version`, and `openai_intent`. The `chat_completions` profile applies only to upstream `/chat/completions` calls, and the `responses` profile applies only to upstream `/responses` calls. Other Copilot upstream requests, including `/models` and readiness probes, use `headers.default` plus global/default fallback values for non-intent headers. `/models` omits `openai-intent` unless it is explicitly configured globally or in `headers.default`; put `openai_intent` in endpoint-specific profiles if you only want it on chat/response requests.

Copilot header profiles only apply to `type: copilot` providers. Non-Copilot providers do not receive configured Copilot headers, and the proxy strips Copilot-identifying client headers such as `authorization`, `editor-version`, `editor-plugin-version`, `user-agent`, `copilot-integration-id`, `x-github-api-version`, `x-request-id`, and `openai-intent` before applying provider-specific authentication.

Routing rules:

- Clients keep using plain model IDs such as `gpt-5.4-pro`.
- Azure `deployment` is the upstream model name; the proxy rewrites the public ID before forwarding.
- Azure `models[]` remains the routing source of truth. The proxy does not autodiscover new Azure deployments for inference.
- Azure is treated as a static provider for `/readyz`: readiness does not depend on Azure's optional `/models` endpoint. `GET /v1/models` may still probe Azure `/models` for best-effort metadata enrichment.
- OpenAI Codex discovers models dynamically from its upstream `/models` endpoint and exposes only models that are listed and supported in the API.
- OpenAI Codex models are `/responses`-only natively. Vekil may serve `/v1/chat/completions`, translated Anthropic, Gemini, count-token probes, and dashboard insights through Chat-over-Responses, while `/v1/models` still reports only native `/responses` support.
- Azure `auth_mode` is optional and defaults to `api_key`. Supported values are `api_key` and `azure_identity`.
- `openai-compatible` models default to `/chat/completions` when `models[].endpoints` is omitted. Add `/responses` only for models you have validated on `responses_path`.
- `anthropic-compatible` models default to `/v1/messages` when `models[].endpoints` is omitted. OpenAI Chat Completions and Responses requests for those models fail fast.
- Generic path fields are `chat_completions_path`, `responses_path`, `messages_path`, and `models_path`. They are paths relative to `base_url`, with no query string or fragment.
- Azure `base_url` must be an absolute URL whose path ends with either the OpenAI-compatible `/openai/v1` path or the legacy `/openai` path, with no query string or fragment.
- Microsoft Foundry inference URLs ending in `/models` are not supported in `type: "azure-openai"` configs. Use the corresponding OpenAI-compatible `.../openai/v1` endpoint instead.
- For `/openai/v1` base URLs, omit `api_version`; the proxy calls `/chat/completions`, `/responses`, and `/models` directly with no `api-version` query string.
- For legacy `/openai` base URLs, set `api_version`; the proxy appends `api-version=...` to upstream requests.
- Public model IDs are global across legacy provider models and explicit routes. Startup fails if ownership collides after supported normalization.
- `include_models` is the recommended way to use dynamic providers without prefixes. It lets you opt into only the discovered model IDs that should belong to that provider.
- `exclude_models` lets one provider give ownership of a public ID to another provider.
- Configuring `include_models` or `exclude_models` on a dynamic provider forces canonical model discovery during initialization, even when it is the only provider; initialization fails if that discovery fails. When discovery is explicitly deferred, `/readyz` remains not ready until `ValidateDynamicProviderModels` completes.
- Unknown-model passthrough is available only on unfiltered dynamic providers. Once `include_models` or `exclude_models` is configured, a request must resolve to a model retained in that provider's canonical discovered catalog or it fails locally with `400`.
- Only a queryless canonical `/v1/models` build may refresh global dynamic model ownership. If the first caller request has a query string, the proxy performs and caches an internal queryless canonical build before returning the query-specific variant; the variant itself never replaces routing state.
- Only one Copilot provider is supported in a config today.
- For Copilot-discovered models, Codex-compatible `/v1/models` metadata treats `capabilities.limits.max_prompt_tokens` as the active `context_window` and keeps `max_context_window_tokens` as `max_context_window`. If Copilot omits the prompt cap, the proxy falls back to the total context window.
- `models[].endpoints` and `model_routes[].endpoints` are native endpoint allowlists, not guesses. Keep them limited to operations validated for the provider model or every target in the route; served Chat compatibility through `/responses` does not add `/chat/completions` to either allowlist.
- `models[].drop_stop_sequences: true` and route-level `model_routes[].drop_stop_sequences: true` are explicit `/chat/completions` compatibility policies. They remove OpenAI `stop` after translation, including Anthropic `stop_sequences`. Because this relaxes public stop semantics, enable it only for upstream models that reject the field and clients that can accept model-determined termination. It does not affect `/responses` requests.
- `models[].use_max_completion_tokens: true` is a request policy for `/chat/completions`; it rewrites `max_tokens` to `max_completion_tokens` after translation without changing the value. Use it for deployments that reject the legacy field, including when Anthropic-compatible clients such as Claude Code supply `max_tokens`. It does not affect `/responses` requests.
- Static provider models can also advertise richer Codex `/v1/models` metadata via optional fields on each `models[]` entry: `model_picker_category`, `reasoning_effort`, `vision`, `parallel_tool_calls`, and `context_window`. Without those fields, the proxy exposes a minimal but valid model entry.
- For Azure OpenAI, `/v1/models` only does a best-effort metadata overlay for each configured `models[]` entry by probing Azure's upstream `/models` response. The proxy matches by `public_id` first, then by `deployment` for aliased models.
- Azure's upstream `/models` catalog can omit Codex-style fields entirely. The proxy only copies fields that Azure already returns; it does not derive reasoning levels, vision, parallel tool calls, model picker metadata, or context window from other Azure docs or capability hints.
- Explicit `models[]` metadata overrides Azure `/models` overlay metadata. Configured public IDs and endpoint allowlists always win, and the proxy falls back to the static entry if the Azure `/models` probe fails or returns a sparse payload.
- The example Azure `gpt-5.4-pro` model shown above is `/responses`-only natively. Chat-compatible clients can use Vekil's adapter, but do not advertise `/chat/completions` in its endpoint allowlist unless the Azure deployment itself has verified native Chat support.

Use the examples above as a starting point for your local providers config file. JSON and YAML use the same snake_case field names.
