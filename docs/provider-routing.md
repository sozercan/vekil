# Provider Routing and Authentication

Use this file when editing provider credentials, model ownership, JSON/YAML provider configs, provider header profiles, or provider-specific model metadata. For global flags and env vars, see [Configuration](configuration.md).

## Provider Authentication

### GitHub Copilot

For CI or other non-interactive environments, set `COPILOT_GITHUB_TOKEN` to a GitHub token for a user with GitHub Copilot access. This is the only GitHub token environment variable Vekil consumes directly; it overrides cached Vekil login state and is exchanged for a short-lived Copilot token at startup.

Vekil intentionally ignores generic GitHub token variables such as `GH_TOKEN` and `GITHUB_TOKEN`. If you want Vekil to use an authenticated GitHub CLI account, opt in explicitly with `vekil login --github-cli` or `vekil login --gh`; Vekil then runs `gh auth token --hostname github.com` for Copilot access and keeps that token in memory only, without copying it into Vekil's `access-token` or `api-key.json` caches.

Plain `vekil login` refreshes an existing Vekil-managed login when possible, otherwise starts GitHub's device-code flow. Use `vekil login --force` to force a new device-code flow even if an existing login can still refresh. A device-code sign-in disables GitHub CLI auto sign-in because the active account is then managed by Vekil rather than by `gh`.

After `vekil logout` or menubar Sign Out, Vekil clears its cached credentials, disables GitHub CLI auto sign-in, and suppresses automatic GitHub CLI reuse until you explicitly opt back in with `vekil login --github-cli` or `vekil login --gh`. `COPILOT_GITHUB_TOKEN` remains an explicit override and still works while signed out.

### OpenAI Codex

OpenAI Codex uses the ChatGPT/Codex CLI credentials in `~/.codex/auth.json` by default. Set `CODEX_HOME` if your Codex home lives elsewhere.

OpenAI Codex requires file-based ChatGPT auth from `codex login`; API-key auth and OS keychain-backed credentials are not read by the proxy.

### Azure OpenAI

Azure OpenAI credentials are configured in the provider entry, using either `api_key` or `api_key_env`.

## Provider Routing

Use `--providers-config` when you want explicit ownership of public model IDs across providers such as GitHub Copilot, Azure OpenAI, and OpenAI Codex. Provider config files can be JSON (`.json`) or YAML (`.yaml`/`.yml`).

You can run Azure-only or Codex-only configs, or mix those providers with Copilot behind the same local endpoint.

### Azure-Only Example

```yaml
providers:
  - id: azure-openai
    type: azure-openai
    default: true
    base_url: https://myresource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
```

### Copilot + Azure Example

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    exclude_models:
      - gpt-5.4-pro
  - id: azure-openai
    type: azure-openai
    base_url: https://myresource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
```

### OpenAI Codex Subscription Example

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
  - id: openai-codex
    type: openai-codex
    include_models:
      - gpt-5.5
```

JSON configs use the same snake_case field names as YAML.

### Copilot Provider Header Profiles

`type: copilot` providers can define a `headers` block with a provider-wide `default` profile and endpoint-specific `chat_completions` and `responses` profiles. Endpoint-specific values override `headers.default`, which overrides the global Copilot header flags/environment variables, which then fall back to the built-in defaults. Omitted fields inherit from the lower-precedence profile. The built-in `openai-intent: conversation-panel` fallback is endpoint-aware: it is applied to upstream `/chat/completions` and `/responses` calls, while upstream `/models` calls send `openai-intent` only when you configure it explicitly through `--copilot-openai-intent`, `COPILOT_OPENAI_INTENT`, or a provider header profile.

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    headers:
      default:
        editor_version: vscode/1.95.0
        editor_plugin_version: copilot-chat/0.26.7
        user_agent: GitHubCopilotChat/0.26.7
        copilot_integration_id: vscode-chat
        github_api_version: "2025-05-01"
      chat_completions:
        openai_intent: conversation-panel
      responses:
        openai_intent: agent-mode
```

Supported header fields are `editor_version`, `editor_plugin_version`, `user_agent`, `copilot_integration_id`, `github_api_version`, and `openai_intent`. The `chat_completions` profile applies only to upstream `/chat/completions` calls, and the `responses` profile applies only to upstream `/responses` calls. Other Copilot upstream requests, including `/models` and readiness probes, use `headers.default` plus global/default fallback values for non-intent headers. `/models` omits `openai-intent` unless it is explicitly configured globally or in `headers.default`; put `openai_intent` in endpoint-specific profiles if you only want it on chat/response requests.

Copilot header profiles only apply to `type: copilot` providers. Non-Copilot providers do not receive configured Copilot headers, and the proxy strips Copilot-identifying client headers such as `authorization`, `editor-version`, `editor-plugin-version`, `user-agent`, `copilot-integration-id`, `x-github-api-version`, `x-request-id`, and `openai-intent` before applying provider-specific authentication.

Routing rules:

- Clients keep using plain model IDs such as `gpt-5.4-pro`.
- Azure `deployment` is the upstream model name; the proxy rewrites the public ID before forwarding.
- Azure `models[]` remains the routing source of truth. The proxy does not autodiscover new Azure deployments for inference.
- OpenAI Codex discovers models dynamically from its upstream `/models` endpoint and exposes only models that are listed and supported in the API.
- OpenAI Codex models are `/responses`-only. The proxy rejects `/chat/completions` for those models instead of probing an unsupported route.
- Azure `base_url` must be an absolute URL whose path ends with either the OpenAI-compatible `/openai/v1` path or the legacy `/openai` path, with no query string or fragment.
- Azure AI Foundry inference URLs ending in `/models` are not supported in `type: "azure-openai"` configs. Use the corresponding OpenAI-compatible `.../openai/v1` endpoint instead.
- For `/openai/v1` base URLs, omit `api_version`; the proxy calls `/chat/completions`, `/responses`, and `/models` directly with no `api-version` query string.
- For legacy `/openai` base URLs, set `api_version`; the proxy appends `api-version=...` to upstream requests.
- Public model IDs are global across all providers. Startup fails if two providers expose the same ID.
- `include_models` is the recommended way to use dynamic providers without prefixes. It lets you opt into only the discovered model IDs that should belong to that provider.
- `exclude_models` lets one provider give ownership of a public ID to another provider.
- Only one Copilot provider is supported in a config today.
- For Copilot-discovered models, Codex-compatible `/v1/models` metadata treats `capabilities.limits.max_prompt_tokens` as the active `context_window` and keeps `max_context_window_tokens` as `max_context_window`. If Copilot omits the prompt cap, the proxy falls back to the total context window.
- `models[].endpoints` is an allowlist, not a guess. Keep it limited to the routes you have validated for that deployment.
- Static provider models can also advertise richer Codex `/v1/models` metadata via optional fields on each `models[]` entry: `model_picker_category`, `reasoning_effort`, `vision`, `parallel_tool_calls`, and `context_window`. Without those fields, the proxy exposes a minimal but valid model entry.
- For Azure OpenAI, `/v1/models` only does a best-effort metadata overlay for each configured `models[]` entry by probing Azure's upstream `/models` response. The proxy matches by `public_id` first, then by `deployment` for aliased models.
- Azure's upstream `/models` catalog can omit Codex-style fields entirely. The proxy only copies fields that Azure already returns; it does not derive reasoning levels, vision, parallel tool calls, model picker metadata, or context window from other Azure docs or capability hints.
- Explicit `models[]` metadata overrides Azure `/models` overlay metadata. Configured public IDs and endpoint allowlists always win, and the proxy falls back to the static entry if the Azure `/models` probe fails or returns a sparse payload.
- The example Azure `gpt-5.4-pro` model shown above is `/responses`-only. Do not advertise `/chat/completions` for that model unless you have verified native support.

Use the examples above as a starting point for your local providers config file. JSON and YAML use the same snake_case field names.
