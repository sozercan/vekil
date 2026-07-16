# Traffic Dashboard

Vekil serves a live, browser-based traffic dashboard from the proxy itself. Because it is part of the HTTP server, it is available **wherever the proxy runs** — the macOS/Linux tray app, a headless `vekil` server, or a container — not just the desktop app.

Open it at:

```
http://<host>:<port>/dashboard
```

For a default local run that is <http://localhost:1337/dashboard>. In the tray app, **Open Dashboard** launches the same URL in your browser once Vekil is running.

## What it shows

The dashboard polls `GET /stats.json` once per second and renders:

- **Cards** — in-flight requests, requests/sec, tokens/sec, error rate, latency p50/p95, upstream retries, and cumulative tokens.
- **Time series** — requests/sec (with an errors/sec overlay) and tokens/sec (prompt vs. completion) over a rolling window, drawn with [uPlot](https://github.com/leeoniya/uPlot).
- **Total usage** — cumulative requests, total/prompt/completion tokens, cached-prompt %, reasoning tokens, average tokens per request, and errors.
- **Breakdowns** — top models, providers, and agents, each with request count, token volume, error count, and average latency. A controls bar lets you **sort by** requests / tokens / errors / latency and **filter** by name (e.g. `gpt-5.4-pro` to inspect one model). Sorting by errors or latency hides rows with none. The JSON snapshot additionally includes `by_route` client-request rows and `by_target` physical-attempt rows for external tooling.
- **Errors** — a status-class distribution (2xx/3xx/4xx/5xx) plus exact error status codes and error-by-provider/model attribution.
- **Recent requests** — a drill-down log of the most recent logical requests (newest first) with status, model, agent, latency, tokens, and the final/canonical upstream request ID. Has an *errors only* toggle, honors the breakdown filter, and lets you click a request ID to copy it for correlating with upstream logs. Each JSON row also carries `operation_id`, `route_id`, `final_target`, `upstream_sends`, and `target_switches` when route data exists.

**Agents** are classified from the request `User-Agent` (for example Claude Code, Codex CLI, Gemini CLI, GitHub Copilot, curl) so you can see which client is driving traffic. Only the friendly label is retained, never the raw `User-Agent`.

**Upstream retries** retain their existing meaning: same-target retries performed by legacy provider routes on transient upstream failures. A version-2 route target switch is counted separately in `target_switches`; it does not redefine or inflate `retries`.

Route/failover metrics are additive in `GET /stats.json`: `upstream_attempts`, `target_switches`, `requests_with_failover`, `successful_failovers`, `route_exhaustions`, `state_binding_hits`, `state_binding_misses`, and `state_binding_evictions`. `by_route` adds client-request aggregates plus failover counters; `by_target` reports physical send counts. The websocket bridge still records each provider-backed `response.create` once in client totals, but its route-level client row/recent-row enrichment is limited: websocket physical sends and switches appear in `upstream_attempts`, `target_switches`, and `by_target`, while `requests_with_failover`, `successful_failovers`, and `by_route` are populated from HTTP request summaries.

## AI insights (optional)

When an insight model is configured, the dashboard shows a **Generate insights** button. Clicking it sends the current traffic snapshot to that model — through Vekil's own `/v1/chat/completions` path — and renders a short natural-language analysis in a block below the live narrative. The model is told what the static narrative already shows and asked to add to it (error concentration, cost shape, trends, a recommendation) rather than repeat it.

Enable it by setting `insight_model` in your providers config to a public model ID the config serves:

```yaml
insight_model: claude-opus-4.8
providers:
  - id: anthropic
    # ...serves claude-opus-4.8
```

See [`examples/anthropic-with-insights.yaml`](../examples/anthropic-with-insights.yaml) for a complete example. When `insight_model` is unset the button is hidden and `GET /stats.json` reports `insights_enabled: false`.

Notes:

- **Opt-in.** No model is called unless you configure one. The button does not appear otherwise.
- **It spends tokens.** Each click is one short chat-completion against the configured model.
- **Rate-limited.** The endpoint is single-flight (one generation at a time) with a short cooldown between generations, so repeat or concurrent clicks cannot fan out billable calls.
- **Fails open.** Any error (no model, timeout, upstream failure, rate-limit) returns a soft error and the dashboard keeps showing its templated narrative.
- **Self-excluded.** The insight call runs in-process and does not pass through the stats middleware, so it is not counted in traffic and cannot feed back on itself.

## Data model

Routing observability has two related ledgers:

1. The **client-request ledger** records exactly one row/outcome for an inbound HTTP request or WebSocket `response.create` turn. It owns client-visible status, latency, requested public model, provider, and accepted-turn usage. HTTP explicit-route summaries also carry route, final target, send count, and switch count; websocket turns currently expose their route topology through the physical-attempt counters instead.
2. The **physical-attempt ledger** increments `upstream_attempts` for each explicit-route inference send and `by_target[].attempts` for the selected route/target/provider. Target switches and route exhaustion are separate from legacy same-target retries. A bounded internal attempt trace retains outcome/decision and per-attempt request IDs for diagnostics; those details are not aggregate labels or recent-row fields.

A successful failover still increments client request totals once. Existing token totals remain client-request/accepted-turn accounting rather than physical-send totals. The recent-request row exposes only the final/canonical upstream request ID.

Metrics are aggregated in memory in the proxy and reset when the process restarts; nothing is persisted. Only inference and compatibility endpoints that produce model completions are counted. The dashboard's own requests (`/dashboard`, its assets, `/stats.json`, `/dashboard/insight`) and the `/healthz` and `/readyz` probes are excluded so the dashboard does not measure itself. Also excluded are non-generating or metadata routes whose traffic would otherwise dilute the LLM-usage metrics: model-catalog reads (`GET /v1/models`), token-counting probes (`POST /v1/messages/count_tokens` and Gemini `:countTokens`, which are non-generating and often served from cache or a local estimate), and the proxy-owned compatibility shims (`POST /v1/responses/compact`, `POST /v1/memories/trace_summarize`).

Token usage is captured across all inference surfaces: OpenAI chat completions (streaming and non-streaming), Anthropic messages, Gemini, and the OpenAI Responses API used by Codex — including the proxy-owned `GET /v1/responses` websocket bridge. A structurally valid provider terminal outcome is recorded immediately and exactly once; a client-first abort without a terminal outcome is one 499, while shutdown-first lifecycle cancellation is intentionally excluded from provider traffic. The Responses `input_tokens`/`output_tokens` shape (with cached and reasoning details) is mapped onto the same prompt/completion fields as chat. Tokens spent by proxy-internal websocket auto-compaction or replay `413` compaction are folded into the owning create turn, including spend accumulated before a later compaction parse/merge failure; post-terminal auto-compaction amends the existing turn instead of creating another request. Failed provider turns retain terminal partial usage, and provider attribution remains tied to the route that actually served the turn even if a dynamic catalog changes before completion. The long-lived `GET /v1/responses` websocket connection itself is not counted as a request (it would otherwise pin the in-flight gauge and skew latency); its individual turns are.

`GET /stats.json` returns a snapshot:

```json
{
  "uptime_seconds": 1234,
  "inflight": 3,
  "totals": {
    "requests": 5000, "errors": 12,
    "prompt_tokens": 1000000, "completion_tokens": 500000, "total_tokens": 1500000,
    "cached_tokens": 300000, "reasoning_tokens": 80000,
    "latency_p50_ms": 240, "latency_p95_ms": 1300, "latency_p99_ms": 2100
  },
  "status": { "2xx": 4988, "4xx": 8, "5xx": 4 },
  "status_codes": [ { "label": "429", "count": 6 }, { "label": "400", "count": 2 } ],
  "errors": [ { "label": "copilot / gpt-5.4", "count": 8 } ],
  "series": [ { "t": 1718900000, "req": 2, "err": 0, "prompt": 1500, "completion": 800 } ],
  "by_model":    [ { "model": "claude-sonnet-4.5", "requests": 200, "tokens": 500000, "errors": 3, "avg_ms": 320 } ],
  "by_provider": [ { "provider": "copilot", "kind": "copilot", "requests": 4000, "tokens": 1200000, "errors": 9, "avg_ms": 300 } ],
  "by_route":    [ { "route": "gpt-5-4-route", "requests": 200, "tokens": 80000, "errors": 2, "avg_ms": 410,
                      "target_switches": 12, "requests_with_failover": 10, "successful_failovers": 8, "route_exhaustions": 2 } ],
  "by_target":   [ { "route": "gpt-5-4-route", "target": "secondary", "provider": "azure-east", "kind": "azure-openai", "attempts": 12 } ],
  "by_agent":    [ { "agent": "Claude Code", "requests": 3200, "tokens": 900000, "avg_ms": 310 } ],
  "retries": 14,
  "retries_by_code": [ { "label": "429", "count": 12 }, { "label": "transport", "count": 2 } ],
  "upstream_attempts": 5012,
  "target_switches": 12,
  "requests_with_failover": 10,
  "successful_failovers": 8,
  "route_exhaustions": 2,
  "state_binding_hits": 120,
  "state_binding_misses": 3,
  "state_binding_evictions": 1,
  "recent": [ { "t": 1718900000, "endpoint": "openai_chat", "model": "gpt-5.4",
               "operation_id": "5dc6cb8d-3e12-43a5-a822-5e89c68c7a40", "route_id": "gpt-5-4-route", "final_target": "secondary",
               "provider": "azure-east", "agent": "Codex CLI", "status": 200, "dur_ms": 1234,
               "total_tokens": 900, "upstream_sends": 2, "target_switches": 1,
               "upstream_request_id": "req-abc" } ],
  "insights_enabled": true
}
```

Breakdowns return up to 25 rows (so the client can re-sort/filter and still see rows outside the top-by-requests); the recent log holds the last ~80 requests; latency percentiles are computed over a bounded recent sample. Breakdown maps are capped in cardinality — a client sending unbounded distinct model names or User-Agents folds into an `other` bucket rather than growing memory without limit. Route, target, and provider labels come only from bounded validated configuration.

Failure/suppression reasons are closed enums. Raw error text, provider state, response/turn-state IDs, upstream request IDs, credentials, and actual version strings are never aggregate labels. Provider error/header detail is redacted and length-bounded before logging. Attempt topology and per-attempt request IDs are also omitted from the optional dashboard insight prompt.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/dashboard` | The dashboard HTML page |
| `GET` | `/dashboard/{asset}` | Vendored uPlot assets (`uPlot.min.js`, `uPlot.min.css`) |
| `GET` | `/stats.json` | The traffic snapshot above |
| `POST` | `/dashboard/insight` | Generate an AI insight (only when `insight_model` is configured) |

## Access and security

`/dashboard`, `/stats.json`, and `/dashboard/insight` are **unauthenticated**, like `/healthz`. Vekil binds to `127.0.0.1` by default, so they are reachable only from the local machine. If you bind to a non-loopback address (for example `0.0.0.0` in a container with a published port), these become reachable by anyone who can reach that port — put it behind your own network controls in that case. This matters most for `/dashboard/insight`, which spends tokens; it is rate-limited but still unauthenticated, so do not expose it publicly without your own access controls.

## Charting dependency

The charts use uPlot, vendored as two static files (`uPlot.min.js` + `uPlot.min.css`) embedded into the binary via `go:embed` and served same-origin from `/dashboard/`. There is no CDN dependency (the dashboard works offline), no build step, and no entry in `go.mod`. uPlot is MIT licensed.
