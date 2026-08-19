# Architecture

## System Shape

```text
┌──────────────────────────────────────────────────────────────────────┐
│                                vekil                                 │
│                                                                      │
│  OpenAI Chat ───────────► Public model-entry registry                │
│                                  │                                   │
│                      ┌───────────┴───────────┐                       │
│                      │                       │                       │
│                 static entry          policy profile                │
│                      │             facts + off/observe/enforce       │
│                      │                       │                       │
│                      └───────────┬───────────┘                       │
│                                  ▼                                   │
│                         Sealed terminal route                        │
│                                  ▼                                   │
│                         Shared route executor                        │
│                                  ├─► Copilot                         │
│  Anthropic/Gemini ─► canonical Chat ├─► Azure                       │
│  Direct Responses ─► native path    ├─► Codex                       │
│                                      └─► Generic                     │
│                                                                      │
│  /v1/responses/compact + /v1/memories/... ─► Responses shims         │
│  auth + provider state ─► login/token caches + exact state binding   │
└──────────────────────────────────────────────────────────────────────┘
```

Chat-compatible ingress converges before provider I/O: public OpenAI Chat, translated Anthropic Messages, translated Gemini generation, their count-token probes, dashboard insights, and bounded policy Responses input all submit canonical Chat requests to the same execution layer. Schema-v2 policy resolution is entered by OpenAI Chat, translated Anthropic Messages/counting, and stateless policy Responses; Gemini and ordinary direct Responses remain outside policy selection. Direct `anthropic-compatible` forwarding and non-policy `/v1/responses` remain separate paths.

## Package Responsibilities

| Package | Responsibility |
|---------|---------------|
| `main` | CLI dispatch, server setup, shared startup authentication, graceful shutdown |
| `launch/` | ephemeral proxy supervision, agent adapters, child environment sanitization, and session summaries |
| `auth/` | GitHub OAuth device code flow, Copilot token exchange, disk caching, auto-refresh |
| `proxy/` | HTTP handlers, public-entry/terminal-route registries, policy planning/classification, provider routing, Anthropic/OpenAI and Gemini/OpenAI translation, Responses compatibility, optional tool optimizer hooks, SSE streaming, retry logic, and provider-specific request/auth helpers outside GitHub OAuth |
| `models/` | Request and response type definitions only |
| `logger/` | Structured JSON logging |
| `server/` | Reusable HTTP server lifecycle |
| `cmd/menubar/` | macOS/Linux tray app |

## Public Entry and Terminal Route Registries

Routing resolves a client-visible model ID to a logical model route rather than directly to one provider model. A route separates:

- the public model contract and catalog metadata;
- an ordered list of physical provider/model targets;
- route-wide request policy;
- target-specific, semantics-preserving wire adaptation; and
- legacy retry compatibility.

Provider-only version-1 configuration remains compatible. Static and dynamically discovered provider models compile to one-target legacy routes, preserving their existing ownership, unknown-model, catalog, and same-target retry behavior. A version-2 `model_routes` entry compiles to an explicit route whose target order is configuration-owned. Registry snapshots are immutable after publication; a dynamic legacy-catalog refresh that would collide with an explicit public entry is rejected as a whole, leaving the last-known-good snapshot active.

Schema v2 makes the registry split explicit:

- The **terminal route registry** is keyed by operational route ID and contains both public and internal routes.
- The **public model-entry registry** is keyed by public ID/alias and contains static route entries plus policy profile entries.

Only the public registry resolves client model IDs. Internal routes have no public aliases, do not appear in `/v1/models`, and cannot be used as dashboard insight models. A policy profile references terminal route IDs, but a policy may not reference another policy. This prevents operational provider/route/target/deployment IDs from accidentally becoming public model aliases.

Public identity stays route-owned in the catalog: `/v1/models` exposes one entry for an explicit route, and target IDs, deployment names, and temporary target availability do not become separate public models or mutate catalog metadata. Explicit Responses output, websocket metadata, and translated Anthropic/Gemini output use the public ID. Legacy OpenAI Chat normalization remains conservative and can preserve a nonempty provider-supplied `model`. Explicit routes normalize supported Chat JSON and SSE model fields and model headers to the route public ID.

For a policy entry, public identity is profile-owned. `/v1/models` exposes one `owned_by: vekil-policy` entry with `/chat/completions` metadata; normalized Chat, translated Anthropic, and adapted stateless Responses output, safe model headers, errors, and metrics retain the profile public ID even when an internal terminal route executes through Responses-backed Chat. Internal provider, terminal-route, target, and deployment identities are bounded operational provenance and do not leak through normalized output.

## Policy Planning Seam

Canonical Chat ingress sees policy routing through one planner seam. The planner owns public-entry lookup, effective mode resolution, bounded fact construction, observer/classifier admission, classifier-route health, deterministic signal mapping, terminal route selection, and bounded decision provenance. It returns a sealed operation plan containing copied candidate bindings and separate public-profile and terminal-route identity.

The plan is sealed before first-send admission and contains no prompt text, tool arguments, credentials, or classifier rationale. The handler executes the returned plan rather than rescanning route targets or reading terminal public identity independently.

Classifier sends reuse provider request/auth primitives but are separate auxiliary operations with their own timeout, non-blocking global/per-profile concurrency admission, infrastructure breaker, and one-send budget. They do not consume the selected terminal route's target-attempt or upstream-send budgets. After selection, the existing route executor remains the sole owner of physical target ordering, request/model rewriting, replay-safe failover, forced streaming/aggregation, cleanup, error selection, and response normalization.

V1 policy planning is stateless and single-tenant: no downstream identity, tenant isolation, session header, affinity cache, or shared cross-process policy state exists. A non-loopback policy deployment is accepted only with the explicit remote-single-tenant acknowledgement, which adds no authentication.

## Attempt Execution and Replay Safety

Explicit routes use one route executor beneath the public handlers. Request handling has two isolation boundaries:

1. **Once per logical operation:** parse and validate one canonical request, run route-agnostic translation, and apply opt-in tool optimization once. For a policy request, policy planning seals the selected tier, terminal route, and optional tier-owned effort; that effort replaces canonical Chat `reasoning_effort` before native-Chat versus Responses-backed execution is chosen. The resulting body bytes and sanitized client headers are immutable operation inputs.
2. **Once per target attempt:** start from those immutable inputs, rewrite the physical model/deployment, apply only semantics-preserving target/provider wire policy, construct a fresh URL/request, and add only that target's authentication and headers. Target selection never recomputes or changes policy-tier effort.

A body, header map, credential, response header, or upstream request ID from one target is never reused for another target. Attempts are serialized; a failed body and its local reader/pump must be closed before another target can be selected. Inference dispatch also rejects redirects and disables implicit request-body replay so one reserved send corresponds to one physical dispatch.

Each logical operation has one total inference deadline and two independent hard budgets:

- `max_target_attempts` counts distinct target selections, including the first target;
- `max_upstream_sends` counts every physical inference POST, including bounded same-target protocol recovery and integrated compaction or compatibility calls.

Policy classifier sends are outside these terminal budgets. The selected terminal operation still has exactly the same route-executor budget and replay-safety rules as a direct request to that route. Failure never crosses from lightweight to powerful or vice versa.

The deadline is not restarted per target. A hanging primary can consume the total deadline; the initial implementation does not promise a secondary attempt in that case. An ordinary HTTP client disconnect closes admission for any *new* attempt without changing the established cancellation behavior of the active attempt. WebSocket close, missed pong, deadline, and process shutdown cancel active session work and prevent another attempt.

A target switch is allowed only when every safety condition is known to hold: retry admission is open, the shared deadline and both budgets have capacity, delivery is definitely replay-safe, no semantic/tool progress or downstream commitment has occurred, no exact state binding requires the previous target, cleanup has completed, and neither client lifecycle nor server shutdown prevents more work. Unknown evidence fails closed.

Attempts classify three monotonic dimensions rather than relying on a single `committed` flag:

| Dimension | States relevant to routing |
|-----------|----------------------------|
| Request delivery | definitely not delivered, explicitly rejected before execution, or delivered/ambiguous |
| Upstream progress | none, allowed preamble, semantic output, tool activity, terminal success/failure, or unknown |
| Downstream commitment | none, headers/protocol frame, or semantic output |

Only a definitely-not-delivered or adapter-certified explicit rejection, with progress limited to none/allowed preamble and downstream commitment still none, can move to another target. Resets after write, generic gateway errors, partial success bodies, malformed/unknown stream events, emitted protocol frames, semantic output, and tool activity are not replay-safe by default.

## Exact Provider-State Binding

Provider-issued continuation state is an exact ownership constraint, not a routing hint. Adapter-marked values such as `previous_response_id`, trusted `X-Codex-Turn-State`, non-proxy opaque `encrypted_content`, and other opaque reasoning/session artifacts are bound to `{route_id, target_id}` immediately before exposure.

All state inputs on a request must resolve together to the same route and target. Known state pins the exact target and disables failover. Conflicting, malformed, cross-route, mixed known/unknown, or unknown state on an explicit route fails locally without an upstream call. If the same token is ever observed with different owners, the store records a conflict tombstone and continues to fail it closed rather than choosing one owner. An unavailable bound target also fails closed; Vekil does not guess another owner or migrate provider state.

Bindings use keyed digests and live in a process-local index capped at 262,144 entries with a 24-hour absolute TTL. Raw state is not used as a log field or metrics label. Expiry, eviction, process restart, or routing a continuation to another Vekil process turns the binding into unknown state. Stateful multi-target routes therefore require a single Vekil process or sticky ingress to the process that owns the binding map. Durable/shared bindings and cross-target replay or session migration are not implemented.

## Key Decisions

- Pure `net/http` with Go `ServeMux` routing; no web framework.
- Vekil is a multi-provider proxy. Zero-config startup currently uses GitHub Copilot, but explicit provider config can extend or replace that default behind the same public surface.
- Public model IDs are a single namespace across legacy provider models and explicit model routes. The proxy validates normalized ownership during startup and fails fast on collisions.
- Schema-v2 policy profiles share that public namespace but select only between `lightweight` and `powerful` terminal routes that execute canonical Chat through native Chat or bounded Chat-over-Responses. Internal routes are operational-only and exposed public terminal routes deliberately bypass policy.
- Policy routing is quality/cost optimization, not authorization or spend enforcement. Its public contract is text/function-tool canonical Chat with translated Anthropic and bounded stateless Responses ingress; selection remains per-turn, with process-local replay bound to its originating route/tier and downstream-bridge replay limited to the documented single-target `off`/`observe` exception, and is limited to one trusted user/tenant per deployment.
- Classifier content forwarding, trust-domain crossing, and provider retention require explicit operator acknowledgements. Those acknowledgements and live protocol preflight do not prove an external provider's retention behavior.
- Provider endpoint support is explicit. `models[].endpoints` and `model_routes[].endpoints` are allowlists, so do not expose `/chat/completions` or other routes until every target in the public contract has verified equivalent behavior.
- Gemini is a translation path like Anthropic, not a passthrough path.
- OpenAI Chat Completions is near-zero-copy when the selected model has native Chat support. Responses-native models use an explicit conversion path; unsupported input is rejected rather than silently dropped.
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and optional websocket bridging.
- Ordinary HTTP inference plus proxy-owned internal and model-catalog calls use timeout-bounded contexts detached from the inbound request, so inbound cancellation alone does not abort pre-header upstream work. `Server.Stop` first closes admission: new non-health requests receive a local `503` and are excluded from provider traffic stats, while `/healthz` remains available until the listener closes. It then cancels the `ProxyHandler` lifecycle before websocket draining and `http.Server.Shutdown`, promptly stopping active work and making later upstream contexts immediately canceled. Admitted requests still blocked on incomplete inbound bodies have their client connection closed, detached dashboard-insight workers are joined, websocket drain failures are propagated, and idle upstream transports are closed. Requests terminated specifically by lifecycle transport cancellation are excluded from provider accounting; buffered semantic upstream failures still retain their classified status, and provider turns that completed before shutdown remain counted. Readiness probes remain request-bound, while constructor-time and deferred startup provider validation retain their existing context roots.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support; it is disabled by default and must be enabled explicitly.
- Tool optimizers are opt-in and fail-open. They must remain disabled by default and must not change default passthrough behavior when unconfigured or when an external optimizer fails.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- Generic provider support is config-driven. `openai-compatible` providers use OpenAI Chat Completions and optional Responses paths, while `anthropic-compatible` providers directly forward native Anthropic Messages requests.
- OpenAI Codex subscription support is a Responses-only dynamic provider backed by Codex CLI ChatGPT credentials.
- Explicit terminal routes provide ordered `primary_only` or bounded `priority_failover`, not active-active balancing. There are no weights, affinity/stickiness policy, active terminal health probes, automatic cross-route/tier fallback, or cross-target state migration. The policy classifier has a separate infrastructure-only breaker that does not rank or reroute terminal targets.
- Production dependencies stay minimal.

## Chat Completions over Responses design gate

Chat-compatible public surfaces use a deep Chat execution module at the provider-routing seam. Callers submit canonical Chat JSON and receive either a canonical Chat completion or a typed canonical Chat event stream; provider ownership, cold model discovery, native-Chat-versus-Responses selection, model rewrite, replay restoration, and Responses conversion remain inside the module. The OpenAI Chat handler, translated Anthropic Messages, translated Gemini generation, both translated count-token probes, and dashboard insights share this seam.

The implementation boundary is concentrated in `proxy/chat_execution.go`, `proxy/chat_route*.go`, `proxy/chat_over_responses_*.go`, `proxy/responses_chat_*.go`, and `proxy/chat_stream_events.go`. Public-protocol handlers consume canonical Chat results and do not learn Responses item or event shapes.

Native `/chat/completions` is always preferred when a model supports both native endpoints. A Responses-backed Chat request uses a captured immutable provider/model route and low-level `/responses` transport; it never calls the public Responses handler, compaction path, websocket bridge, lineage reset, or Responses optimizer pipeline. Provider model endpoint metadata remains native and is not expanded with emulated Chat support.

Phase 0 compared two post-Responses-decoding stream transports with 10,000 text deltas and eight interleaved function calls:

- Option A synthesized Chat SSE and parsed it again through the existing Chat stream reader.
- Option B carried typed internal Chat events to the public-protocol adapters.

On the Phase 0 machine, the combined median was 31.72 ms, 13.40 MB, and 216,966 allocations for Option A versus 1.15 ms and no incremental transport allocations for Option B. Option A exceeded the allowed 20% elapsed and 25% allocation thresholds, so the implementation uses **typed internal Chat events**. Both prototypes passed the 850 ms first-byte bound, bounded-buffer checks, and 1,000 cancellation-cleanup iterations.

The initial frozen limits are:

```text
provider-local route discovery timeout: 2 seconds
successful discovery TTL: 5 minutes
failed discovery backoff: 5 seconds
stream precommit timeout: 750 milliseconds
stream precommit prefix: 64 KiB
single Responses SSE event: 8 MiB
converted Responses JSON: 16 MiB
replay TTL: 1 hour
replay groups: 2,048
replay group bytes: 2 MiB
replay total bytes: 64 MiB
replay output items per group: 256
replay function calls per group: 128
```

Replay-backed function calls expose either a versioned, checksummed self-describing `call_vekil_v2_<15-character-base64url-nonce>_<upstream-call-id>_<4-character-checksum>` ID, which embeds Copilot's own call ID and resolves with no lookup, or the opaque `call_vekil_<22-character-base64url>` fallback, which is minted when the upstream ID is ineligible, would exceed Anthropic's 64-character limit, or the preferred self-describing ID collides. The nonce is random per published replay group; only the checksum is deterministic for that nonce and upstream ID. The checksum distinguishes minted replay IDs from plausible native call IDs but is integrity, not authorization. The decoder also accepts the older `call_vekil_v1_<upstream-call-id>_<8-character-checksum>` form for existing transcripts, but the minter no longer emits it. Resolution is bound to the captured provider, public model, upstream model, complete assistant content, and complete ordered tool-call projection. The assistant call array cannot be shortened or reordered, but a complete set of tool-result messages may arrive in any order. For a non-empty partial result set, the adapter restores only the matching prior function calls and outputs; this is an empirical compatibility requirement because replaying the complete parallel call group with only partial outputs was rejected upstream. Missing calls may be reissued.

Replay state is process-local and expires one hour after capture; reads do not extend that deadline. It is also bounded by group count, per-group bytes, total bytes, item count, and call count, with LRU eviction under pressure. A restart, expiry, eviction, forged ID, or route mismatch produces the deterministic `responses_replay_state_missing` client error rather than attempting to reconstruct hidden reasoning. The exception is Anthropic ingress **on a direct route**, which opts into rebuilding the turn from the client's own transcript — immediately on a single-target route, and only once every candidate target has already refused it on a multi-target one. It has two modes. A self-describing ID recovers the call mapping, so the turn goes upstream under the `call_id` Copilot issued. A legacy random ID recovers nothing, so the turn goes upstream under the proxy `call_vekil_...` ID instead; that is sound only because the whole turn is replayed in one request with `store: false`, where a `call_id` need agree with its own `function_call_output` and nothing else. Native `/v1/chat/completions` and `/v1/responses` take neither path and keep the deterministic error, and neither does a policy profile on any surface: its tier comes from a carrier tag keyed per process, so `routeSelectingCarriers` drops every carrier from another process and trial resolution fails in the planner, before the Anthropic pass. Neither rebuild carries reasoning. Raw replay content is never logged.

Synthetic fixtures and the final `MAP` / `LOCAL` / `REJECT` request matrix live in [`../proxy/testdata/chat_over_responses/README.md`](../proxy/testdata/chat_over_responses/README.md). Raw live traces remain outside the repository with owner-only permissions.
