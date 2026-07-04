# Documentation Index

This folder is intentionally split into small, single-purpose files so humans and coding agents can load only the topic they need. It covers both provider-agnostic proxy behavior and provider-specific notes for GitHub Copilot, Azure OpenAI, OpenAI Codex, and generic compatible providers.

## Doc Map

| File | Scope | Update When |
|------|-------|-------------|
| [`getting-started.md`](getting-started.md) | install, run, first authentication, deployment entry points | startup flow or distribution changes |
| [`configuration.md`](configuration.md) | configuration map, generic CLI flags/env vars, and Copilot header overrides | generic flags, env vars, or Copilot header defaults change |
| [`provider-routing.md`](provider-routing.md) | provider auth, JSON/YAML routing examples, model ownership, endpoint allowlists | providers, routing behavior, auth, or model metadata changes |
| [`provider-api-keys.md`](provider-api-keys.md) | where to get provider API keys and how to map them into providers config | provider signup/key URLs or auth field guidance changes |
| [`tool-optimizers.md`](tool-optimizers.md) | optional shell command rewrite and tool-output reduction config | optimizer config or behavior changes |
| [`speed-tier-routing.md`](speed-tier-routing.md) | optional speed/cost-tier model downgrade routing | speed-tier config, signals, validation, or logging behavior changes |
| [`responses-websocket.md`](responses-websocket.md) | Codex-style `GET /v1/responses` websocket bridge tuning | websocket bridge, auto-compaction, or compact retry knobs change |
| [`clients.md`](clients.md) | copy-paste client examples | onboarding snippets or client compatibility changes |
| [`api.md`](api.md) | concise endpoint map and compatibility links | public routes are added, removed, or renamed |
| [`gemini.md`](gemini.md) | Gemini translation compatibility details | Gemini request/response translation behavior changes |
| [`responses.md`](responses.md) | OpenAI Responses, compact, and memory shim details | Responses passthrough, compaction, or shim behavior changes |
| [`architecture.md`](architecture.md) | package responsibilities and data flow | implementation boundaries or design decisions change |
| [`dashboard.md`](dashboard.md) | live browser traffic dashboard and `/stats.json` | dashboard metrics, endpoints, or stats behavior change |
| [`menubar.md`](menubar.md) | macOS/Linux tray app usage | tray behavior or packaging changes |
| [`development.md`](development.md) | build, test, benchmark, and CI workflows | local dev or CI commands change |

## Agent Notes

- Prefer linking to one focused file instead of expanding the root `README.md`.
- When behavior changes, update the smallest relevant doc instead of adding more material to the root README.
- Keep each doc narrowly scoped and avoid duplicating long explanations across files.
- Separate provider-agnostic API behavior from provider-specific auth or routing details when possible.
- When documenting provider features, distinguish proxy-owned websocket bridging from upstream-native websocket or realtime APIs.
