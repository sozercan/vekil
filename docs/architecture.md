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

Chat-compatible ingress converges before provider I/O: public OpenAI Chat, translated Anthropic Messages, translated Gemini generation, their count-token probes, and dashboard insights all submit canonical Chat requests to the same execution layer. Direct `anthropic-compatible` forwarding and direct `/v1/responses` remain separate paths.

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

## Key Decisions

- Pure `net/http` with Go `ServeMux` routing; no web framework.
- Vekil is a multi-provider proxy. Zero-config startup currently uses GitHub Copilot, but explicit provider config can extend or replace that default behind the same public surface.
- Public model IDs are a single namespace across providers. The proxy validates ownership during startup and fails fast on collisions.
- Provider endpoint support is explicit. `models[].endpoints` is an allowlist, so do not expose `/chat/completions` or other routes for a provider/model until that upstream capability is verified.
- Gemini is a translation path like Anthropic, not a passthrough path.
- OpenAI Chat Completions is near-zero-copy when the selected model has native Chat support. Responses-native models use an explicit conversion path; unsupported input is rejected rather than silently dropped.
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and optional websocket bridging.
- Ordinary HTTP inference plus proxy-owned internal and model-catalog calls use timeout-bounded contexts detached from the inbound request, so inbound cancellation alone does not abort pre-header upstream work. `Server.Stop` first closes admission: new non-health requests receive a local `503` and are excluded from provider traffic stats, while `/healthz` remains available until the listener closes. It then cancels the `ProxyHandler` lifecycle before websocket draining and `http.Server.Shutdown`, promptly stopping active work and making later upstream contexts immediately canceled. Admitted requests still blocked on incomplete inbound bodies have their client connection closed, detached dashboard-insight workers are joined, websocket drain failures are propagated, and idle upstream transports are closed. Requests terminated specifically by lifecycle transport cancellation are excluded from provider accounting; buffered semantic upstream failures still retain their classified status, and provider turns that completed before shutdown remain counted. Readiness probes remain request-bound, while constructor-time and deferred startup provider validation retain their existing context roots.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support; it is disabled by default and must be enabled explicitly.
- Tool optimizers are opt-in and fail-open. They must remain disabled by default and must not change default passthrough behavior when unconfigured or when an external optimizer fails.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- Generic provider support is config-driven. `openai-compatible` providers use OpenAI Chat Completions and optional Responses paths, while `anthropic-compatible` providers directly forward native Anthropic Messages requests.
- OpenAI Codex subscription support is a Responses-only dynamic provider backed by Codex CLI ChatGPT credentials.
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

Replay-backed function calls expose only opaque `call_vekil_<22-character-base64url>` IDs. Resolution is bound to the captured provider, public model, upstream model, complete assistant content, and complete ordered tool-call projection. The assistant call array cannot be shortened or reordered, but a complete set of tool-result messages may arrive in any order. For a non-empty partial result set, the adapter restores only the matching prior function calls and outputs; this is an empirical compatibility requirement because replaying the complete parallel call group with only partial outputs was rejected upstream. Missing calls may be reissued.

Replay state is process-local and expires one hour after capture; reads do not extend that deadline. It is also bounded by group count, per-group bytes, total bytes, item count, and call count, with LRU eviction under pressure. A restart, expiry, eviction, forged ID, or route mismatch produces the deterministic `responses_replay_state_missing` client error instead of attempting to reconstruct hidden reasoning. Raw replay content is never logged.

Synthetic fixtures and the final `MAP` / `LOCAL` / `REJECT` request matrix live in [`../proxy/testdata/chat_over_responses/README.md`](../proxy/testdata/chat_over_responses/README.md). Raw live traces remain outside the repository with owner-only permissions.
