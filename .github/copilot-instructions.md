# Copilot Instructions

## Build & Test

```bash
go build -o copilot-proxy .          # Build binary
go test ./... -count=1               # Run all tests
go test ./proxy/ -run TestHandle -v  # Run specific test group
go test ./proxy/ -run TestMapStopReason/stop -v  # Run single subtest
go vet ./...                         # Lint
```

## Architecture

This is a Go proxy server that exposes Anthropic and OpenAI-compatible APIs, forwarding all requests to GitHub Copilot's backend (`api.githubcopilot.com`).

**Two distinct request paths:**

- **Anthropic path** (`/v1/messages`): Full translation layer. Incoming Anthropic requests are converted to OpenAI format via `TranslateAnthropicToOpenAI()`, always forwarded with `stream: true` to Copilot's `/chat/completions`, and responses are translated back via `TranslateOpenAIToAnthropic()` (non-streaming) or `StreamOpenAIToAnthropic()` (streaming). Non-streaming responses are aggregated from SSE via `aggregateStreamToResponse()` before translation.

- **OpenAI path** (`/v1/chat/completions`, `/v1/responses`): Near zero-copy passthrough for requests without tools. When tools are present, the proxy injects `parallel_tool_calls: true`, forces `stream: true` upstream, and aggregates the SSE response back to JSON via `aggregateStreamToResponse()`. This works around an upstream limitation where non-streaming responses may drop parallel tool calls.

**Package responsibilities:**

- `auth/` — GitHub OAuth device code flow, Copilot token exchange, disk caching (`~/.config/copilot-proxy/`), auto-refresh with `sync.RWMutex`
- `proxy/` — HTTP handlers, Anthropic↔OpenAI translation, SSE streaming
- `models/` — Request/response type definitions for both APIs (data-only, no logic)

## Conventions

- **No HTTP framework** — uses raw `net/http` with Go 1.22+ routing (`mux.HandleFunc("POST /v1/messages", ...)`) for performance.
- **Pointer types for optional fields** — model structs use `*int`, `*float64`, `*bool` for optional JSON fields with `omitempty`.
- **`json.RawMessage` for polymorphic fields** — Anthropic's `Content` field can be a string or `[]ContentBlock`; handled via `json.RawMessage` with try-string-then-array unmarshaling pattern.
- **Streaming architecture** — OpenAI passthrough copies bytes directly (`StreamOpenAIPassthrough`). Anthropic streaming parses OpenAI SSE chunks line-by-line and emits translated Anthropic events (`StreamOpenAIToAnthropic`). Non-streaming requests are always forced to stream upstream (Anthropic path: always; OpenAI path: when tools present), then aggregated back to JSON via `aggregateStreamToResponse()` for reliable parallel tool call support.
- **Forced-streaming helpers** — `injectParallelToolCalls()` adds `parallel_tool_calls: true` to OpenAI requests with tools (preserves explicit `false`). `injectForceStream()` adds `stream: true` and `stream_options` to force streaming upstream.
- **Auth in handlers** — Every handler calls `h.auth.GetToken(ctx)` to get a valid Copilot token. Tokens auto-refresh; the auth layer handles all caching/locking internally.
- **Test helpers** — `auth.NewTestAuthenticator(token)` creates a pre-loaded authenticator for tests. `newTestProxyHandler(t, backendHandler)` in handler_test.go sets up a handler with a mock Copilot backend via `httptest.NewServer`.
- **Error formats** — Anthropic endpoints return `{"type":"error","error":{"type":"...","message":"..."}}`. OpenAI endpoints return `{"error":{"message":"...","type":"...","code":null}}`.
