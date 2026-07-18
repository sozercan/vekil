# Documentation Index

This folder is intentionally split into small, single-purpose files so humans and coding agents can load only the topic they need. It covers both provider-agnostic proxy behavior and provider-specific notes for GitHub Copilot, Azure OpenAI, OpenAI Codex, and generic compatible providers.

## Doc Map

| File | Scope | Update When |
|------|-------|-------------|
| [`getting-started.md`](getting-started.md) | install, run, first authentication, deployment entry points | startup flow or distribution changes |
| [`configuration.md`](configuration.md) | configuration map, generic CLI flags/env vars, policy-routing runtime ceiling, and Copilot header overrides | generic flags, env vars, or Copilot header defaults change |
| [`provider-routing.md`](provider-routing.md) | provider auth, schema-v2/v3 model routes, ordered priority failover, route exposure, model ownership, native endpoint allowlists | providers, routing behavior, failover safety, auth, or model metadata changes |
| [`policy-routing.md`](policy-routing.md) | schema-v3 semantic policy routing, classifier privacy/trust, modes, fallbacks, metrics, evaluation, and rollout gates | policy schema, classifier behavior, supported surfaces, telemetry, or release gates change |
| [`provider-api-keys.md`](provider-api-keys.md) | where to get provider API keys and how to map them into providers config | provider signup/key URLs or auth field guidance changes |
| [`tool-optimizers.md`](tool-optimizers.md) | optional shell command rewrite and tool-output reduction config | optimizer config or behavior changes |
| [`responses-websocket.md`](responses-websocket.md) | Codex-style `GET /v1/responses` websocket bridge tuning | websocket bridge, auto-compaction, or compact retry knobs change |
| [`clients.md`](clients.md) | copy-paste client examples | onboarding snippets or client compatibility changes |
| [`api.md`](api.md) | public endpoint map plus cross-surface Chat/Responses compatibility contract | public routes or Chat compatibility behavior changes |
| [`gemini.md`](gemini.md) | Gemini translation compatibility details | Gemini request/response translation behavior changes |
| [`responses.md`](responses.md) | native Responses behavior, the Chat-over-Responses boundary, compact, and memory shims | Responses passthrough, Chat adaptation, compaction, or shim behavior changes |
| [`architecture.md`](architecture.md) | package responsibilities, route registry/executor, Chat execution seam, replay safety, and data flow | implementation boundaries or design decisions change |
| [`dashboard.md`](dashboard.md) | live browser traffic dashboard, route/attempt metrics, AI insights, and `/stats.json` | dashboard metrics, endpoints, insight routing, or stats behavior change |
| [`menubar.md`](menubar.md) | macOS/Linux tray app usage | tray behavior or packaging changes |
| [`development.md`](development.md) | build, test, route-safety, policy-routing, and Chat-over-Responses matrices, benchmark/evaluation baselines, and CI workflows | local dev, test gates, benchmark commands, evaluation gates, or CI change |

## Agent Notes

- Prefer linking to one focused file instead of expanding the root `README.md`.
- When behavior changes, update the smallest relevant doc instead of adding more material to the root README.
- Keep each doc narrowly scoped and avoid duplicating long explanations across files.
- Separate provider-agnostic API behavior from provider-specific auth or routing details when possible.
- When documenting provider features, distinguish proxy-owned websocket bridging from upstream-native websocket or realtime APIs.
