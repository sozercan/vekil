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

## Key Decisions

- Pure `net/http` with Go `ServeMux` routing; no web framework.
- Vekil is a multi-provider proxy. Zero-config startup currently uses GitHub Copilot, but explicit provider config can extend or replace that default behind the same public surface.
- Public model IDs are a single namespace across providers. The proxy validates ownership during startup and fails fast on collisions.
- Provider endpoint support is explicit. `models[].endpoints` is an allowlist, so do not expose `/chat/completions` or other routes for a provider/model until that upstream capability is verified.
- Gemini is a translation path like Anthropic, not a passthrough path.
- OpenAI Chat Completions is near-zero-copy except where forced streaming is needed for tool reliability.
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and optional websocket bridging.
- Ordinary HTTP inference plus proxy-owned internal and model-catalog calls use timeout-bounded contexts detached from the inbound request, so inbound cancellation alone does not abort pre-header upstream work. `Server.Stop` first closes admission: new non-health requests receive a local `503` and are excluded from provider traffic stats, while `/healthz` remains available until the listener closes. It then cancels the `ProxyHandler` lifecycle before websocket draining and `http.Server.Shutdown`, promptly stopping active work and making later upstream contexts immediately canceled. Admitted requests still blocked on incomplete inbound bodies have their client connection closed, detached dashboard-insight workers are joined, websocket drain failures are propagated, and idle upstream transports are closed. Requests terminated specifically by lifecycle transport cancellation are excluded from provider accounting; buffered semantic upstream failures still retain their classified status, and provider turns that completed before shutdown remain counted. Readiness probes remain request-bound, while constructor-time and deferred startup provider validation retain their existing context roots.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support; it is disabled by default and must be enabled explicitly.
- Tool optimizers are opt-in and fail-open. They must remain disabled by default and must not change default passthrough behavior when unconfigured or when an external optimizer fails.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- Generic provider support is config-driven. `openai-compatible` providers use OpenAI Chat Completions and optional Responses paths, while `anthropic-compatible` providers directly forward native Anthropic Messages requests.
- OpenAI Codex subscription support is a Responses-only dynamic provider backed by Codex CLI ChatGPT credentials.
- Production dependencies stay minimal.
