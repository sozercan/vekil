# CLAUDE.md

## Project

Go reverse proxy that routes Anthropic, Gemini, and OpenAI API requests to GitHub Copilot's backend (`api.githubcopilot.com`), using a Copilot subscription instead of direct API keys.

## Build & Test

```bash
make build          # build binary
make test           # run all tests (go test ./... -count=1)
make vet            # go vet ./...
make lint           # runs vet
make build-app      # macOS menubar .app bundle
make docker-build   # docker image
```

Run specific tests: `go test ./proxy/ -run TestHandle -v`
Run websocket benchmark: `go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesWebSocketRequestBuild' -benchmem -count=1`
Run transport benchmark: `go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesTransport' -benchmem -count=1`

## Documentation

Documentation is intentionally split under `docs/` into small, single-purpose files:

| File | Scope |
|------|-------|
| `README.md` | Short landing page only |
| `docs/README.md` | Documentation index and doc map |
| `docs/getting-started.md` | install, run, first auth, deployment entry points |
| `docs/configuration.md` | flags, env vars, websocket tuning |
| `docs/clients.md` | copy-paste client examples |
| `docs/api.md` | endpoint behavior and compatibility notes |
| `docs/architecture.md` | package boundaries and design notes |
| `docs/menubar.md` | macOS app usage |
| `docs/development.md` | build, test, benchmark, CI |

Documentation update rules:

- Keep `README.md` concise; put detail into the focused file under `docs/`.
- When behavior changes, update the narrowest relevant doc instead of appending to the root README.
- Prefer linking between docs rather than duplicating long sections.

## Architecture

| Package | Purpose |
|---------|---------|
| `main.go` | CLI entry, flags, signal handling |
| `auth/` | GitHub OAuth device code flow, token caching/refresh (`sync.RWMutex` + double-check) |
| `proxy/chat_handlers.go` | Anthropic and OpenAI chat handlers, including forced-streaming aggregation for tool calls |
| `proxy/responses_handler.go` | OpenAI Responses passthrough plus Codex compatibility endpoints (`/v1/responses/compact`, `/v1/memories/trace_summarize`) |
| `proxy/handler.go` | Shared proxy plumbing: health/ready/models handlers, request-body decoding, Copilot headers, caches |
| `proxy/gemini_handler.go` | Gemini-native HTTP handlers and countTokens probe flow |
| `proxy/gemini.go` | Gemini↔OpenAI request/response translation and validation |
| `proxy/gemini_streaming.go` | OpenAI SSE → Gemini SSE translation |
| `proxy/translator.go` | Bidirectional Anthropic↔OpenAI request/response translation |
| `proxy/streaming.go` | SSE stream translation (OpenAI→Anthropic events) and aggregation |
| `proxy/retry.go` | Exponential backoff on 429/502/503/504 |
| `models/` | Data-only structs for Anthropic and OpenAI API types (no logic) |
| `logger/` | Structured JSON logger to stderr |
| `server/` | HTTP server lifecycle (`Start`/`Stop`/`IsRunning`) |
| `cmd/menubar/` | macOS systray app (separate binary) |

## Key Design Decisions

- **No frameworks**: Pure `net/http` with Go 1.22+ `ServeMux` method routing. Do not add web frameworks.
- **Forced streaming for parallel tool calls**: Non-streaming requests with tools are force-streamed upstream then aggregated back, because Copilot's non-streaming mode doesn't reliably return parallel tool calls. This is the project's core value-add.
- **Gemini is a translation layer**: Gemini endpoints are implemented like Anthropic, not as zero-copy passthrough. Keep Gemini-specific protocol logic in `proxy/gemini*.go`.
- **Responses compatibility is proxy-owned**: `/v1/responses/compact` and `/v1/memories/trace_summarize` are compatibility shims implemented on top of the upstream `/responses` API. Preserve this behavior for Codex-style clients.
- **OpenAI passthrough is near-zero-copy, not literal zero-copy**: chat completions may inject `parallel_tool_calls` and force streaming; `/v1/responses` may rewrite proxy-owned compaction items before forwarding.
- **Minimal dependencies**: Keep third-party deps minimal. Current non-stdlib dependencies used in production code are `systray`, `uuid`, and `klauspost/compress`.
- **Distroless container**: Single static binary, CGO_ENABLED=0.

## Code Conventions

- Error handling: return errors up, log at boundaries (main, handlers). Use `logger.Err(err)` for structured fields.
- Tests: table-driven tests with `httptest` for handler tests. Test files live alongside source. Use `auth.NewTestAuthenticator()` for mock auth in tests.
- `models/` package is data-only — put all logic in `proxy/` or `auth/`.
- Model name normalization: strip date suffixes, map hyphens to dots (e.g. `claude-sonnet-4-5` → `claude-sonnet-4.5`).

## CI

GitHub Actions in `.github/workflows/ci.yaml` runs `golangci-lint` (only new issues), test, build, vet, then e2e (binary smoke test + docker build). All must pass before merge.
