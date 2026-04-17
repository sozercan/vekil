# Architecture

## System Shape

```text
┌─────────────────────────────────────────────────────┐
│                   vekil                     │
│                                                     │
│  /v1/messages ──► Translate ──► /chat/completions   │
│                   Anthropic→OAI   api.githubcopilot │
│                   Translate ◄──   .com              │
│                   OAI→Anthropic                     │
│                                                     │
│  /v1beta/models/... ─► Translate ─► /chat/completions│
│  /models/...          Gemini→OAI     api.githubcopilot│
│                       Translate ◄──   .com           │
│                       OAI→Gemini                     │
│                                                     │
│  /v1/chat/completions ─────────► Provider router ─► │
│  /v1/responses ────────────────► (Copilot / Azure  │
│                   HTTP /responses)                 │
│                   (passthrough + proxy compaction   │
│                    expansion when needed)           │
│  /v1/responses/compact ────────► /responses         │
│  /v1/memories/trace_summarize ─► /responses         │
│                   (proxy-owned Codex compatibility) │
│                                                     │
│  auth/ ── Device code flow ── Token cache & refresh │
└─────────────────────────────────────────────────────┘
```

## Package Responsibilities

| Package | Responsibility |
|---------|---------------|
| `main` | CLI flags, HTTP server setup, graceful shutdown |
| `auth/` | GitHub OAuth device code flow, Copilot token exchange, disk caching, auto-refresh |
| `proxy/` | HTTP handlers, provider routing, Anthropic/OpenAI and Gemini/OpenAI translation, SSE streaming, retry logic |
| `models/` | Request and response type definitions only |
| `logger/` | Structured JSON logging |
| `server/` | Reusable HTTP server lifecycle |
| `cmd/menubar/` | macOS menubar app |

## Key Decisions

- Pure `net/http` with Go `ServeMux` routing; no web framework.
- Public model IDs are a single namespace across providers. The proxy validates ownership during startup and fails fast on collisions.
- Provider endpoint support is explicit. `models[].endpoints` is an allowlist, so do not expose `/chat/completions` or other routes for a provider/model until that upstream capability is verified.
- Gemini is a translation path like Anthropic, not a passthrough path.
- OpenAI Chat Completions is near-zero-copy except where forced streaming is needed for tool reliability.
- OpenAI Responses compatibility is partly proxy-owned, especially for Codex compaction and websocket bridging.
- The Codex websocket bridge is transport adaptation over upstream HTTP `/responses`, not a claim that the selected provider has native websocket or realtime support.
- Azure OpenAI support is implemented as an OpenAI-compatible provider behind the existing proxy surface; Azure deployment names are internal to provider config.
- Production dependencies stay minimal.
