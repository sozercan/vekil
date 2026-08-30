# Provider API Keys

Use this file when you need to find where a provider issues API keys and how that key maps into a Vekil providers config. For routing behavior and full config examples, see [Provider Routing](provider-routing.md).

Provider portals, free tiers, and model catalogs change frequently. Treat the links below as starting points, then verify current terms, quotas, regions, and supported endpoints in the provider's own console.

## Key Handling

- Prefer `api_key_env` over inline `api_key` for any config you might commit or share.
- Use any environment variable name you want; it only needs to match the provider's `api_key_env` value.
- Names like `AZURE_OPENAI_API_KEY` or `PROVIDER_API_KEY` in examples are placeholders, not names Vekil treats specially.
- If `api_key_env` is set in config, that environment variable must be set and non-empty before Vekil starts.
- Local provider configs also support `${env:VAR_NAME}` in string values, but `api_key_env` itself is not interpolated because it already names a variable. Prefer `api_key_env` for credentials; use [provider-config interpolation](configuration.md#environment-interpolation-in-local-provider-configs) for other host-specific values. HTTP(S)-loaded configs cannot interpolate host environment values.
- Use `auth_type: none` only for local providers or a trusted private upstream.
- Keep `models[].endpoints` limited to routes you have validated for that model. Vekil does not infer `/responses` from OpenAI compatibility.
- Do not paste provider keys into client configs. Clients point at Vekil with dummy local keys; Vekil holds the upstream provider credentials.

## Built-In Provider Auth

| Provider | How To Authenticate | Vekil Config |
|----------|---------------------|--------------|
| GitHub Copilot | Run `vekil login`, opt into GitHub CLI auth with `vekil login --github-cli`, or set `COPILOT_GITHUB_TOKEN` for non-interactive runs. | `type: copilot` |
| Azure OpenAI | Create an Azure OpenAI resource/deployment in the Azure Portal or Azure AI Foundry, then copy a resource key. | `type: azure-openai`, `api_key_env: <the env var name you exported>` |
| OpenAI Codex | Run `codex login` so `~/.codex/auth.json` exists. API keys are not used for this provider. | `type: openai-codex` |

## Generic Provider Key Pages

| Provider | Key Page | Typical Vekil Type | Notes |
|----------|----------|--------------------|-------|
| NVIDIA NIM | [build.nvidia.com/settings/api-keys](https://build.nvidia.com/settings/api-keys) | `openai-compatible` | Use `/chat/completions`; add `/responses` only for a validated model. |
| Kimi / Moonshot | [platform.moonshot.ai/console/api-keys](https://platform.moonshot.ai/console/api-keys) | `openai-compatible` | Configure `base_url` exactly as Moonshot documents for the API you use. |
| OpenCode Zen / Go | [opencode.ai/auth](https://opencode.ai/auth) | `openai-compatible` | Prefer `/responses` for models documented for Responses; keep chat separate unless validated. Free models work anonymously with `api_key: public`; see [Provider Routing](provider-routing.md#opencode-zen-free-tier). |
| Z.ai | [z.ai/manage-apikey/apikey-list](https://z.ai/manage-apikey/apikey-list) | `openai-compatible` | Do not append `/v1` unless that is part of the documented API base. |
| OpenRouter | [openrouter.ai/keys](https://openrouter.ai/keys) | `anthropic-compatible` or `openai-compatible` | Pick the provider type that matches the upstream endpoint you configure. |
| DeepSeek | [platform.deepseek.com/api_keys](https://platform.deepseek.com/api_keys) | `anthropic-compatible` or `openai-compatible` | Match `type` and paths to the endpoint family you intend to use. |
| Wafer | [wafer.ai](https://www.wafer.ai/) | `anthropic-compatible` | Use its native Messages endpoint when configuring direct Anthropic routing. |
| Google AI Studio | [aistudio.google.com/app/apikey](https://aistudio.google.com/app/apikey) | `openai-compatible` | Use Google's OpenAI-compatible API base when configuring this as a generic provider. |
| Mistral La Plateforme | [console.mistral.ai](https://console.mistral.ai/) | `openai-compatible` | Use the OpenAI-compatible API base documented by Mistral. |
| Groq | [console.groq.com/keys](https://console.groq.com/keys) | `openai-compatible` | Some models reject unsupported request fields; validate with a small request first. |
| Cerebras Cloud | [cloud.cerebras.ai](https://cloud.cerebras.ai/) | `openai-compatible` | Use the OpenAI-compatible API base from Cerebras docs. |

## Local Providers Without API Keys

| Provider | Typical URL | Vekil Config |
|----------|-------------|--------------|
| LM Studio | `http://localhost:1234/v1` | `type: openai-compatible`, `auth_type: none` |
| llama.cpp server | commonly `http://localhost:8080/v1` | `type: openai-compatible`, `auth_type: none` |
| AIKit | `http://localhost:8080/v1` | `type: openai-compatible`, `auth_type: none` |
| Ollama | `http://localhost:11434` | `type: openai-compatible`, `auth_type: none`, `model_discovery: ollama`, `models_path: /api/tags`, `chat_completions_path: /v1/chat/completions` |

## Config Field Checklist

After creating a key, add it to your environment under any name you choose and reference that exact name from the provider config:

```bash
export PROVIDER_API_KEY=<your-api-key>
```

```yaml
providers:
  - id: hosted-provider
    type: openai-compatible
    base_url: https://provider.example.com/v1
    api_key_env: PROVIDER_API_KEY
```

Use `auth_type: api-key-header` and `auth_header` for providers that do not accept bearer auth. Use `auth_type: none` for local providers. Full routing examples live in [Provider Routing](provider-routing.md#generic-provider-cookbook).

## Quick Validation

After exporting the environment variables referenced by `api_key_env`, start Vekil with the config and check:

```bash
vekil serve --providers-config ./providers.yaml
curl http://localhost:1337/readyz
curl http://localhost:1337/v1/models
```

Then send a small request through the public endpoint listed in `models[].endpoints`. If `/readyz` passes but inference fails, verify the upstream path, model/deployment ID, auth header mode, and whether that model actually supports the requested endpoint.
