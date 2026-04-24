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
│                                                      └─► Codex   │
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
| `proxy/` | HTTP handlers, provider routing, Anthropic/OpenAI and Gemini/OpenAI translation, Responses compatibility, SSE streaming, retry logic, and provider-specific request/auth helpers outside GitHub OAuth |
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
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and websocket bridging.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- OpenAI Codex subscription support is a Responses-only dynamic provider backed by Codex CLI ChatGPT credentials.
- Production dependencies stay minimal.
