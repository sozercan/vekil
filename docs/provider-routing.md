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

### Azure OpenAI and Microsoft Foundry

Azure providers support two auth modes:

- **API key auth**: omit `auth_mode` or set `auth_mode: api_key`, then configure either `api_key` or `api_key_env`. This preserves the existing behavior and sends Azure's `api-key` header upstream.
- **Microsoft Entra auth**: set `auth_mode: azure_identity`. Vekil uses the Azure SDK `DefaultAzureCredential` chain, sends `Authorization: Bearer <token>`, and does not send an `api-key` header. Do not configure `api_key` or `api_key_env` in this mode.

For Entra auth, `token_scope` is optional and defaults to `https://ai.azure.com/.default`, which is appropriate for Microsoft Foundry OpenAI-compatible endpoints. Override it if your resource requires a different Azure audience, such as `https://cognitiveservices.azure.com/.default` for classic Azure OpenAI deployments.

Vekil does not run `az login` for you. For local development, sign in with Azure CLI or another credential supported by `DefaultAzureCredential`; in hosted environments, use managed identity, workload identity, or environment credentials. The signed-in principal needs the required Azure RBAC role, for example Cognitive Services OpenAI User on the target resource.

### Generic Providers

`openai-compatible` and `anthropic-compatible` providers use the generic auth fields:

- `auth_type: bearer` sends `Authorization: Bearer <key>` by default.
- `auth_type: api-key-header` sends the key through `auth_header`, with optional `auth_prefix`.
- `auth_type: none` sends no auth header, useful for local providers.
- `extra_headers` adds fixed provider headers after client Copilot-identifying headers are stripped.

When `auth_type` is omitted, Vekil uses `bearer` if `api_key` or `api_key_env` is set, otherwise `none`.

## Provider Routing

Use `--providers-config` when you want explicit ownership of public model IDs across providers such as GitHub Copilot, Azure OpenAI, OpenAI Codex, or generic OpenAI-compatible and Anthropic-compatible upstreams. Provider config files can be JSON (`.json`) or YAML (`.yaml`/`.yml`).

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

### Microsoft Foundry Entra Example

```yaml
providers:
  - id: foundry
    type: azure-openai
    default: true
    auth_mode: azure_identity
    # Optional; defaults to https://ai.azure.com/.default
    token_scope: https://ai.azure.com/.default
    base_url: https://myresource.services.ai.azure.com/api/projects/myproject/openai/v1
    models:
      - public_id: gpt-5.4
        deployment: gpt-5.4
        endpoints:
          - /responses
        name: GPT-5.4
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

### Generic Provider Behavior

OpenAI-compatible providers route `POST /v1/chat/completions` to `chat_completions_path`. Anthropic `POST /v1/messages` requests for these models are translated through the existing OpenAI Chat Completions adapter. `POST /v1/responses` is never inferred; add `/responses` to a model only after validating the upstream model and path.

Anthropic-compatible providers directly forward Anthropic `POST /v1/messages` to `messages_path`. They do not serve OpenAI Chat Completions or Responses routes.

`model_discovery` can be `static`, `openai`, `ollama`, or `openrouter-tools`. OpenAI discovery reads an OpenAI-style `data` array. Ollama discovery reads `/api/tags`. OpenRouter-tools discovery reads an OpenAI/OpenRouter-style `data` array and exposes only models that advertise tool parameters.

### Generic Provider Field Reference

| Field | Applies To | Purpose |
|-------|------------|---------|
| `type` | all providers | Use `openai-compatible` or `anthropic-compatible` for generic providers. |
| `base_url` | generic providers | Upstream origin and any fixed API prefix. The proxy appends only the configured path field. |
| `api_key`, `api_key_env` | generic providers | Static credential value or environment variable name. |
| `auth_type` | generic providers | `bearer`, `api-key-header`, or `none`. Defaults to `bearer` when a key is present, otherwise `none`. |
| `auth_header`, `auth_prefix` | generic providers | Header name and optional prefix for `api-key-header`, or overrides for bearer auth. |
| `extra_headers` | generic providers | Fixed headers to add to every upstream request after client Copilot headers are stripped. |
| `chat_completions_path` | `openai-compatible` | Upstream path for public `POST /v1/chat/completions`. Defaults to `/chat/completions`. |
| `responses_path` | `openai-compatible` | Upstream path for public `POST /v1/responses`. Defaults to `/responses`; models must still opt in with `/responses`. |
| `messages_path` | `anthropic-compatible` | Upstream path for public `POST /v1/messages`. Defaults to `/v1/messages`. |
| `models_path` | generic providers | Upstream path for dynamic model discovery and readiness probes. Defaults to `/models`. |
| `model_discovery` | generic providers | `static`, `openai`, `ollama`, or `openrouter-tools`. |
| `models[].endpoints` | all static models | Public endpoint allowlist. This remains the source of truth for what Vekil advertises and routes. |

### Generic Provider Cookbook

Chat-only OpenAI-compatible hosted providers, including NVIDIA NIM and Kimi, should expose only `/chat/completions` unless you have validated `/responses` for that exact model:

```yaml
providers:
  - id: hosted-chat
    type: openai-compatible
    default: true
    base_url: https://provider.example.com/v1
    api_key_env: PROVIDER_API_KEY
    models:
      - public_id: provider-chat-model
        deployment: upstream-chat-model
        endpoints:
          - /chat/completions
```

LM Studio and llama.cpp usually fit the same shape with local auth disabled:

```yaml
providers:
  - id: local-openai
    type: openai-compatible
    default: true
    base_url: http://localhost:1234/v1
    auth_type: none
    models:
      - public_id: local-chat
        deployment: local-model
        endpoints:
          - /chat/completions
```

Z.ai-style OpenAI-compatible providers can use the same config, but set `base_url` exactly to the upstream API base documented by the provider. Do not append `/v1` unless the provider's OpenAI-compatible base URL includes it.

Ollama can use `/api/tags` discovery and OpenAI-compatible chat routing:

```yaml
providers:
  - id: ollama
    type: openai-compatible
    default: true
    base_url: http://localhost:11434
    auth_type: none
    model_discovery: ollama
    models_path: /api/tags
    chat_completions_path: /v1/chat/completions
```

OpenAI-compatible providers with validated Responses support, including OpenCode Zen/Go models documented for Responses, should opt in per model:

```yaml
providers:
  - id: responses-provider
    type: openai-compatible
    default: true
    base_url: https://provider.example.com/v1
    api_key_env: PROVIDER_API_KEY
    models:
      - public_id: responses-model
        deployment: upstream-responses-model
        endpoints:
          - /responses
```

Anthropic-compatible providers with native Messages support, including Wafer, OpenRouter, and DeepSeek-style Messages endpoints, should not advertise OpenAI routes:

```yaml
providers:
  - id: native-messages
    type: anthropic-compatible
    default: true
    base_url: https://provider.example.com
    api_key_env: PROVIDER_API_KEY
    auth_type: api-key-header
    auth_header: x-api-key
    messages_path: /v1/messages
    models:
      - public_id: claude-compatible
        deployment: upstream-messages-model
        endpoints:
          - /v1/messages
```

If LM Studio, llama.cpp, or Ollama exposes a native Anthropic Messages endpoint in your local setup, configure it as a separate `anthropic-compatible` provider with `messages_path`. Do not rely on `openai-compatible` to direct-forward Messages; that type translates Anthropic requests through Chat Completions.

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
- Azure `auth_mode` is optional and defaults to `api_key`. Supported values are `api_key` and `azure_identity`.
- `openai-compatible` models default to `/chat/completions` when `models[].endpoints` is omitted. Add `/responses` only for models you have validated on `responses_path`.
- `anthropic-compatible` models default to `/v1/messages` when `models[].endpoints` is omitted. OpenAI Chat Completions and Responses requests for those models fail fast.
- Generic path fields are `chat_completions_path`, `responses_path`, `messages_path`, and `models_path`. They are paths relative to `base_url`, with no query string or fragment.
- Azure `base_url` must be an absolute URL whose path ends with either the OpenAI-compatible `/openai/v1` path or the legacy `/openai` path, with no query string or fragment.
- Microsoft Foundry inference URLs ending in `/models` are not supported in `type: "azure-openai"` configs. Use the corresponding OpenAI-compatible `.../openai/v1` endpoint instead.
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
