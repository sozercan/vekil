# 0009: Keep credential ownership separate from child processes

## Context

GitHub tokens and cached Copilot bearers have different lifetimes and endpoint support. Launcher clients also persist configuration across turns, and may inherit ambient credentials belonging to an unrelated provider or process.

## Decision

- Prefer explicitly supplied GitHub credentials to persisted state. Borrow the GitHub CLI credential only when no usable Vekil login exists, not after explicit sign-out or a transient exchange failure. Never assign a legacy bearer of unproven provenance to the current login.
- Use direct GitHub bearers where supported. Copilot Responses may require a short-lived exchange for a GitHub App user token; cache that fallback in memory, keyed to the active credential. Share refresh work without tying it to the first waiter's deadline, and fence publication across sign-out with a generation change.
- Strip ambient provider credentials and endpoint overrides from the launched client. Pass the launch-scoped proxy secret through the client environment, not command-line arguments or persisted configuration. Create temporary configuration with owner-only permissions from the first open.
- Keep the injected Codex `model_provider` ID stable across launches: existing sessions record this identity and will not resume if the provider disappears. Only substitute Claude Code's hyphenated model spelling if proxy resolution proves it denotes the same model. Dotted Anthropic names can otherwise cause Claude Code to send an incompatible thinking mode; substituting a spelling must not shadow a raw or policy ID.

## Consequences

Local status is a snapshot, not a network or GitHub CLI probe. Credential changes and launch-to-launch resume are separate from child configuration lifetime. See [Agent Launchers](../agent-launchers.md) and [Provider Authentication](../provider-routing.md#provider-authentication).
