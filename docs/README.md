# Documentation Index

This folder is intentionally split into small, single-purpose files so humans and coding agents can load only the topic they need. It covers both provider-agnostic proxy behavior and provider-specific notes for GitHub Copilot, Azure OpenAI, and OpenAI Codex.

## Doc Map

| File | Scope | Update When |
|------|-------|-------------|
| [`getting-started.md`](getting-started.md) | install, run, first authentication, deployment entry points | startup flow or distribution changes |
| [`configuration.md`](configuration.md) | CLI flags, env vars, provider routing, websocket tuning | flags, defaults, providers, or runtime knobs change |
| [`clients.md`](clients.md) | copy-paste client examples | onboarding snippets or client compatibility changes |
| [`api.md`](api.md) | endpoint behavior and compatibility notes | request/response semantics change |
| [`architecture.md`](architecture.md) | package responsibilities and data flow | implementation boundaries or design decisions change |
| [`menubar.md`](menubar.md) | macOS/Linux tray app usage | tray behavior or packaging changes |
| [`development.md`](development.md) | build, test, benchmark, and CI workflows | local dev or CI commands change |

## Agent Notes

- Prefer linking to one focused file instead of expanding the root `README.md`.
- When behavior changes, update the smallest relevant doc instead of adding more material to the root README.
- Keep each doc narrowly scoped and avoid duplicating long explanations across files.
- Separate provider-agnostic API behavior from provider-specific auth or routing details when possible.
- When documenting provider features, distinguish proxy-owned websocket bridging from upstream-native websocket or realtime APIs.
