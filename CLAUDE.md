# Contributor Guide

## Project

Go reverse proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs behind one local endpoint. Vekil can run in zero-config mode against GitHub Copilot, or use explicit provider routing to send selected models to configured upstreams such as Azure OpenAI and OpenAI Codex. The public API surface stays the same while provider ownership of models is configured behind the proxy.

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
Run session benchmark: `go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesSession' -benchmem -count=1`
Run Chat-over-Responses tests: `go test ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1`
Run Chat-over-Responses race tests: `go test -race ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1`
Run Chat-over-Responses benchmark: `go test ./proxy/ -run '^$' -bench 'BenchmarkChatOverResponses' -benchmem -count=3`

## Documentation

Per-doc scope and update triggers live in `docs/README.md` (the authoritative doc map). Keep that file indexed whenever docs are added, removed, or behavior changes.

Documentation update rules:

- Keep `README.md` concise; put detail into the focused file under `docs/`.
- When behavior changes, update the narrowest relevant doc instead of appending to the root README.
- Prefer linking between docs rather than duplicating long sections.
- Separate provider-agnostic behavior from provider-specific auth and routing details when possible.

## Architecture

| Area | Purpose |
|------|---------|
| `main.go`, `launch_command.go`, `server/`, `auth/`, `logger/`, `models/`, `cmd/menubar/` | CLI/server lifecycle, GitHub auth, structured logging, data-only API structs, tray app |
| `launch/` | Ephemeral proxy supervision, agent adapters, child environment sanitization, and session summaries |
| `proxy/chat_handlers.go`, `proxy/translator.go`, `proxy/streaming.go`, `proxy/openai_stream_reader.go` | Anthropic/OpenAI chat translation, native-Chat forced-stream aggregation, SSE translation and passthrough |
| `proxy/chat_execution.go`, `proxy/chat_route*.go`, `proxy/chat_over_responses_*.go`, `proxy/responses_chat_*.go`, `proxy/chat_stream_events.go` | Deep Chat execution seam, native-Chat/Responses selection, strict Chat-to-Responses conversion, typed canonical stream events, and bounded tool replay |
| `proxy/gemini*.go` | Gemini-native handlers plus Gemini↔OpenAI request/response and streaming translation |
| `proxy/providers.go`, `proxy/model_routes.go`, `proxy/model_routes_config.go`, `proxy/route_executor.go`, `proxy/provider_endpoint_policy.go`, `proxy/upstream_http.go`, `proxy/openai_codex_auth.go`, `proxy/azure_identity_auth.go` | Provider config, logical route ownership, ordered target execution, endpoint allowlists, model rewrite, provider auth, upstream request construction |
| `proxy/responses_handler.go`, `proxy/responses_websocket.go`, `proxy/responses_failure_translate.go`, `proxy/response_model_normalization.go`, `proxy/route_state_binding.go`, `proxy/state_binding.go`, `proxy/compaction.go` | OpenAI Responses passthrough, compact/memory shims, proxy-owned websocket bridge, exact target state binding, compaction/replay behavior, Responses failure translation |
| `proxy/tool_optimizer*.go`, `proxy/optimizer_*.go`, `proxy/tool_output_context.go`, `proxy/tool_shapes.go`, `proxy/filter_hint.go` | Optional fail-open tool command/output optimizers and tool-shape/context helpers |
| `proxy/retry.go`, `proxy/upstream_error_detail.go`, shared helpers in `proxy/handler.go` | Retry/backoff, upstream error summaries, request body limits, headers, health/ready/models handlers, caches |

New `proxy/*.go` files should join one of these responsibility clusters. Add a new row only when a genuinely new responsibility appears.

## Key Design Decisions

- **No frameworks**: Pure `net/http` with Go 1.22+ `ServeMux` method routing. Do not add web frameworks.
- **Vekil is a multi-provider proxy**: zero-config startup currently targets GitHub Copilot, but explicit JSON/YAML provider configs can extend or replace that default behind the same public API surface.
- **Public model IDs are global across providers**: Model ownership is explicit and startup must fail on collisions rather than silently shadowing one provider with another.
- **Deep Chat execution seam**: OpenAI Chat plus translated Anthropic, Gemini, count-token, and dashboard-insight traffic submits canonical Chat requests to one executor. Prefer native `/chat/completions`; otherwise use native `/responses` when allowed. Keep Responses concepts out of the public protocol handlers.
- **Forced streaming for reliable parallel tool calls**: Non-streaming requests with tools may be force-streamed upstream then aggregated back before returning to the client. This applies to both native Chat and Responses-backed Chat execution.
- **Gemini is a translation layer**: Gemini endpoints are implemented like Anthropic, not as zero-copy passthrough. Keep Gemini-specific protocol logic in `proxy/gemini*.go`.
- **Responses compatibility is proxy-owned**: `/v1/responses/compact` and `/v1/memories/trace_summarize` are compatibility shims implemented on top of the upstream `/responses` API. Preserve this behavior for Codex-style clients.
- **OpenAI passthrough is near-zero-copy, not literal zero-copy**: chat completions may inject `parallel_tool_calls` and force streaming; `/v1/responses` may rewrite proxy-owned compaction items before forwarding.
- **Provider endpoint support is native and explicit**: `models[].endpoints` and rendered `supported_endpoints` describe verified upstream routes only. Do not add `/chat/completions` merely because Vekil can serve Chat compatibility through `/responses`; the Azure `gpt-5.4-pro` example remains `/responses`-only metadata.
- **Responses-backed Chat is strict**: Unsupported/unknown fields fail explicitly, non-empty `stop` is rejected (no local stop emulation), and only function tools are supported. Hosted tools are not translated through this path. Preserve Chat's non-storage default with `store: false` when omitted, and request encrypted reasoning internally for stateless tool replay.
- **Responses-backed tool replay is process-local**: Expose only opaque `call_vekil_<22-character-base64url>` IDs. Preserve the full ordered assistant tool-call projection; accept complete results in any order and use per-call replay for non-empty partial result sets. Replay expires after one hour, is byte/item/group bounded while stream deltas are consumed, and is lost on restart.
- **Responses-backed streams use typed internal events**: Do not synthesize Chat SSE only to parse it again. Convert Responses events once and feed canonical events directly to OpenAI, Anthropic, and Gemini adapters.
- **Azure support is OpenAI-compatible provider routing, not a separate public surface**: Azure deployment names stay internal to provider config, Azure auth is provider-configured as either API-key or SDK-backed Entra auth, and Azure `/models` probing is only a best-effort metadata overlay for configured models.
- **OpenAI Codex support is file-auth-backed provider routing**: Codex models use the CLI ChatGPT auth file, dynamic `/models` discovery, and `/responses`-only routing.
- **Proxy websocket bridging is not upstream realtime**: `GET /v1/responses` remains a proxy-owned websocket transport over upstream HTTP `/responses`. Do not describe it as native Azure websocket or `/realtime` support.
- **Tool optimizers are opt-in and fail-open**: keep them disabled by default. External optimizer errors, timeouts, invalid JSON, or invalid replacements must fall back to the original payload and preserve default passthrough behavior.
- **Minimal dependencies**: Keep third-party deps minimal and justify new production dependencies by the feature boundary they support.
- **Distroless container**: Single static binary, `CGO_ENABLED=0`.

## Code Conventions

- Error handling: return errors up, log at boundaries (main, handlers). Use `logger.Err(err)` for structured fields.
- Tests: table-driven tests with `httptest` for handler tests. Test files live alongside source. Use `auth.NewTestAuthenticator()` for mock auth in tests.
- `models/` package is data-only — put all logic in `proxy/` or `auth/`.
- Model name normalization: strip date suffixes, map hyphens to dots (e.g. `claude-sonnet-4-5` → `claude-sonnet-4.5`).

## CI

GitHub Actions in `.github/workflows/ci.yaml` runs `golangci-lint` (only new issues), test, build, vet, then e2e (binary smoke test + docker build). All must pass before merge.

Live smoke workflows are separate from `ci.yaml` so the core gate stays deterministic and credential-free:

- `live-copilot-smoke.yaml` — credentialed Copilot smoke for compaction, Responses-backed Chat text/tool replay, and Codex/Claude/Gemini CLIs. Needs `COPILOT_GITHUB_TOKEN`; self-skips on fork PRs and Dependabot.
- `live-zen-smoke.yaml` — credential-free OpenCode Zen free-tier smoke. Runs `scripts/live-cli-smoke.sh` in `SMOKE_PROVIDER=zen` mode (Copilot CLI offline, Claude, Gemini) and runs on every PR including forks. Neutral-skips when the Zen free tier is unreachable so it never blocks unrelated PRs.
