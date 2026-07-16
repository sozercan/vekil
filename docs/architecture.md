# Architecture

## System Shape

```text
┌──────────────────────────────────────────────────────────────────┐
│                              vekil                               │
│                                                                  │
│  /v1/messages ─┐                                                 │
│  /v1beta/... ──┼─► Translate to OpenAI payloads ─► Provider router│
│  /models/... ──┤                                   │             │
│  /v1/chat/... ─┤                                   ├─► Copilot   │
│  /v1/responses ┘                                   ├─► Azure     │
│                                                      ├─► Codex   │
│                                                      └─► Generic │
│                                                                  │
│  /v1/responses/compact ─┐                                        │
│  /v1/memories/... ──────┴─► Proxy-owned Responses compatibility  │
│                                                                  │
│  auth + provider state ─► GitHub device flow, token caches,      │
│                           Codex auth.json refresh helpers         │
└──────────────────────────────────────────────────────────────────┘
```

## Package Responsibilities

| Package | Responsibility |
|---------|---------------|
| `main` | CLI flags, HTTP server setup, graceful shutdown |
| `auth/` | GitHub OAuth device code flow, Copilot token exchange, disk caching, auto-refresh |
| `proxy/` | HTTP handlers, provider routing, Anthropic/OpenAI and Gemini/OpenAI translation, Responses compatibility, optional tool optimizer hooks, SSE streaming, retry logic, and provider-specific request/auth helpers outside GitHub OAuth |
| `models/` | Request and response type definitions only |
| `logger/` | Structured JSON logging |
| `server/` | Reusable HTTP server lifecycle |
| `cmd/menubar/` | macOS/Linux tray app |

## Model Route Registry

Routing resolves a client-visible model ID to a logical model route rather than directly to one provider model. A route separates:

- the public model contract and catalog metadata;
- an ordered list of physical provider/model targets;
- route-wide request policy;
- target-specific, semantics-preserving wire adaptation; and
- legacy retry compatibility.

Provider-only version-1 configuration remains compatible. Static and dynamically discovered provider models compile to one-target legacy routes, preserving their existing ownership, unknown-model, catalog, and same-target retry behavior. A version-2 `model_routes` entry compiles to an explicit route whose target order is configuration-owned. Registry snapshots are immutable after publication; a dynamic legacy-catalog refresh that would collide with an explicit route is rejected as a whole, leaving the last-known-good snapshot active.

Public identity stays route-owned in the catalog: `/v1/models` exposes one entry for an explicit route, and target IDs, deployment names, and temporary target availability do not become separate public models or mutate catalog metadata. Explicit Responses output, websocket metadata, and translated Anthropic/Gemini output use the public ID. Legacy OpenAI Chat normalization remains conservative and can preserve a nonempty provider-supplied `model`. Explicit routes normalize supported Chat JSON and SSE model fields and model headers to the route public ID.

## Attempt Execution and Replay Safety

Explicit routes use one route executor beneath the public handlers. Request handling has two isolation boundaries:

1. **Once per logical operation:** parse and validate one canonical request, run route-agnostic translation and opt-in tool optimization once, apply uniform route policy, and retain immutable body bytes plus sanitized client headers.
2. **Once per target attempt:** start from those immutable inputs, rewrite the physical model/deployment, apply only semantics-preserving target policy, construct a fresh URL/request, and add only that target's authentication and headers.

A body, header map, credential, response header, or upstream request ID from one target is never reused for another target. Attempts are serialized; a failed body and its local reader/pump must be closed before another target can be selected. Inference dispatch also rejects redirects and disables implicit request-body replay so one reserved send corresponds to one physical dispatch.

Each logical operation has one total inference deadline and two independent hard budgets:

- `max_target_attempts` counts distinct target selections, including the first target;
- `max_upstream_sends` counts every physical inference POST, including bounded same-target protocol recovery and integrated compaction or compatibility calls.

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

Bindings use keyed digests and live in a process-local index capped at 32,768 entries with a 24-hour absolute TTL. Raw state is not used as a log field or metrics label. Expiry, eviction, process restart, or routing a continuation to another Vekil process turns the binding into unknown state. Stateful multi-target routes therefore require a single Vekil process or sticky ingress to the process that owns the binding map. Durable/shared bindings and cross-target replay or session migration are not implemented.

## Key Decisions

- Pure `net/http` with Go `ServeMux` routing; no web framework.
- Vekil is a multi-provider proxy. Zero-config startup currently uses GitHub Copilot, but explicit provider config can extend or replace that default behind the same public surface.
- Public model IDs are a single namespace across legacy provider models and explicit model routes. The proxy validates normalized ownership during startup and fails fast on collisions.
- Provider endpoint support is explicit. `models[].endpoints` and `model_routes[].endpoints` are allowlists, so do not expose `/chat/completions` or other routes until every target in the public contract has verified equivalent behavior.
- Gemini is a translation path like Anthropic, not a passthrough path.
- OpenAI Chat Completions is near-zero-copy except where forced streaming is needed for tool reliability.
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and optional websocket bridging.
- Ordinary HTTP inference plus proxy-owned internal and model-catalog calls use timeout-bounded contexts detached from the inbound request, so inbound cancellation alone does not abort pre-header upstream work. `Server.Stop` first closes admission: new non-health requests receive a local `503` and are excluded from provider traffic stats, while `/healthz` remains available until the listener closes. It then cancels the `ProxyHandler` lifecycle before websocket draining and `http.Server.Shutdown`, promptly stopping active work and making later upstream contexts immediately canceled. Admitted requests still blocked on incomplete inbound bodies have their client connection closed, detached dashboard-insight workers are joined, websocket drain failures are propagated, and idle upstream transports are closed. Requests terminated specifically by lifecycle transport cancellation are excluded from provider accounting; buffered semantic upstream failures still retain their classified status, and provider turns that completed before shutdown remain counted. Readiness probes remain request-bound, while constructor-time and deferred startup provider validation retain their existing context roots.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support; it is disabled by default and must be enabled explicitly.
- Tool optimizers are opt-in and fail-open. They must remain disabled by default and must not change default passthrough behavior when unconfigured or when an external optimizer fails.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- Generic provider support is config-driven. `openai-compatible` providers use OpenAI Chat Completions and optional Responses paths, while `anthropic-compatible` providers directly forward native Anthropic Messages requests.
- OpenAI Codex subscription support is a Responses-only dynamic provider backed by Codex CLI ChatGPT credentials.
- Explicit routes provide ordered `primary_only` or bounded `priority_failover`, not active-active balancing. There are no weights, affinity/stickiness policy, active health probes, shared circuit-breaker domains, automatic cross-route fallback, or cross-target state migration.
- Production dependencies stay minimal.
