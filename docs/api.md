# API Reference

## `POST /v1/messages` (Anthropic)

Anthropic Messages compatibility for the supported content and tool subset. Requests are translated to OpenAI Chat Completions, routed through the provider that owns the selected public model, and translated back to Anthropic.

Supported features:

- text, image-input, and tool-use content blocks
- system messages as string or content block array
- tool definitions and tool choice: `auto`, `any`, `tool`
- stop sequences
- extended thinking via `thinking.type: "enabled"`
- streaming Anthropic SSE event translation

Model normalization:

- dated suffixes are stripped automatically, for example `claude-sonnet-4-20250514`
- hyphenated version numbers are mapped to dotted form, for example `claude-sonnet-4-5` to `claude-sonnet-4.5`

## `GET /v1/models`

The proxy builds a merged model catalog across the active providers. It preserves the OpenAI-style `data` payload and also adds a Codex-compatible top-level `models` array.

```bash
curl http://localhost:1337/v1/models
```

When multiple providers are configured:

- public model IDs stay unprefixed, for example `gpt-5.4`
- each public ID must be owned by exactly one provider
- startup fails if providers collide on the same model ID
- dynamic providers such as Copilot or OpenAI Codex can be narrowed with `include_models` or `exclude_models`
- static providers such as Azure OpenAI can expose a deployment under a different public ID while the proxy rewrites the upstream `model` field

The exact catalog depends on your configured providers and current upstream availability. Query `/v1/models` in your own deployment instead of hard-coding one global model list.

## `POST /v1beta/models/{model}:generateContent` and `POST /models/{model}:generateContent` (Gemini)

Gemini is implemented as a translation layer, not a zero-copy passthrough layer. Gemini requests are translated to OpenAI Chat Completions, routed through the provider that owns the selected public model, and translated back into Gemini responses.

The decoder accepts both standard Gemini camelCase fields and LiteLLM-style snake_case aliases such as `system_instruction`, `function_declarations`, `inline_data`, `max_output_tokens`, and `response_json_schema`.

Supported subset:

- `systemInstruction.parts[].text`
- `contents[].parts[].text`
- `contents[].parts[].inlineData` for `image/*`
- `contents[].parts[].functionCall`
- `contents[].parts[].functionResponse`
- `tools[].functionDeclarations` with `parameters` or `parametersJsonSchema`
- `toolConfig.functionCallingConfig`
- `generationConfig.temperature`, `topP`, `maxOutputTokens`, `stopSequences`
- `generationConfig.responseMimeType`, `responseSchema`, `responseJsonSchema`
- `generationConfig.presencePenalty`, `frequencyPenalty`, `seed`

Accepted but ignored because upstream has no equivalent:

- `generationConfig.topK`
- `generationConfig.thinkingConfig`

Explicit `501 UNIMPLEMENTED` cases include:

- `generationConfig.candidateCount != 1`
- `generationConfig.responseModalities`
- `generationConfig.speechConfig`
- `generationConfig.imageConfig`
- `generationConfig.mediaResolution`
- `generationConfig.responseLogprobs`
- `generationConfig.logprobs`
- `cachedContent`
- `safetySettings`
- multimodal `functionResponse.parts`
- Gemini built-in tools such as `googleSearch`, `urlContext`, `codeExecution`, `googleMaps`, `computerUse`, and `enterpriseWebSearch`
- non-image `inlineData`, `fileData`, and other non-text/media parts

Validation failures (`400 INVALID_ARGUMENT`) include path/body model mismatches, malformed content parts, invalid function-call history, and unmatched `functionResponse` parts.

## `POST /v1beta/models/{model}:countTokens` and `POST /models/{model}:countTokens` (Gemini)

`countTokens` normalizes the Gemini request into the same prompt/tool payload used by `generateContent`, performs a minimal upstream `/chat/completions` probe, and returns `usage.prompt_tokens` as Gemini `totalTokens`. Normalized requests are cached for 60 seconds.

## `POST /v1/chat/completions` (OpenAI)

Near zero-copy passthrough for requests without tools. When tools are present, the proxy injects `parallel_tool_calls: true`, forces upstream streaming for reliable parallel tool calls, and aggregates the result back to non-streaming JSON.

When provider routing is configured, the request is routed by the public `model` ID. If the selected provider uses a different upstream model or deployment name, the proxy rewrites the outgoing `model` field before forwarding.

The proxy also enforces the model's configured `supported_endpoints` before forwarding. If a model is exposed as `/responses`-only, `POST /v1/chat/completions` fails fast with `400` instead of probing an unsupported upstream route. The Azure `gpt-5.4-pro` example configuration and OpenAI Codex subscription models are set up this way.

## `POST /v1/responses` and `GET /v1/responses` (OpenAI)

`POST /v1/responses` is a near zero-copy passthrough for the OpenAI Responses API. Proxy-owned synthetic compaction items are expanded back into normal context before forwarding so resumed Codex sessions continue through the standard `/v1/responses` path.

Like chat completions, Responses requests are routed by the public `model` ID. Fallback retries for unsupported `/responses` models stay within the selected provider; the proxy does not silently switch providers.

For responses-only Azure deployments such as the `gpt-5.4-pro` example configuration, this is the canonical inference path.

Streaming Responses requests preserve upstream headers and are otherwise passed through directly. One narrow exception exists for `POST /v1/responses` with `stream: true`: if the first semantic SSE event is an immediate transient `response.failed` admission error such as `too_many_requests`, `model_overloaded`, `bad_gateway`, or `gateway_timeout`, the proxy translates that pre-commit failure into a normal HTTP error before flushing `200 OK`. All other streaming failures stay passthrough SSE.

`GET /v1/responses` upgrades to a websocket bridge for Codex-style clients. The proxy:

- accepts `response.create` frames
- handles warmup and incremental follow-up requests locally
- forwards the active turn to upstream HTTP `/responses`
- relays streamed JSON events back as websocket text frames

Websocket bridge behavior:

- history is stored append-only in memory
- long sessions are auto-compacted into one proxy-owned checkpoint plus a recent tail
- optional turn-state delta replay can be enabled with `--responses-ws-turn-state-delta`
- if upstream rejects delta replay, the proxy automatically falls back to full replay
- if the first streamed upstream event is a transient `response.failed` admission error, the bridge sends a wrapped websocket error frame instead of relaying the raw `response.failed` event

This websocket bridge is a proxy transport adaptation layered over upstream HTTP `/responses`. It is not the same feature as provider-native websocket or realtime APIs such as Azure `/realtime`.

See [configuration.md](configuration.md) for tuning flags.

## `POST /v1/responses/compact`

Compatibility shim for environments expecting `/responses/compact`. The proxy rewrites the request into a normal upstream `/responses` call with a compaction prompt, then returns:

- a normal assistant summary message
- a proxy-owned opaque `compaction` item whose `encrypted_content` can later be sent back to `/v1/responses` or `/v1/responses/compact`

Requests to this endpoint are accepted up to `64 MiB` so large session histories can be compacted without tripping the default request-body limit.

If the requested model does not support the upstream Responses API, the proxy retries against a compatible fallback model discovered from `/models`.
That fallback stays within the selected provider; the proxy does not silently switch providers.

## `POST /v1/memories/trace_summarize`

Compatibility shim that summarizes one or more traces into `{trace_summary, memory_summary}` objects using the upstream `/responses` endpoint plus a JSON-only summarization prompt.

Requests to this endpoint are accepted up to `64 MiB` so larger trace bundles can be summarized in a single call.

## `GET /healthz`

Returns `{"status":"ok"}`.

## `GET /readyz`

Validates that the proxy can authenticate to and successfully probe the configured upstream providers. On success it returns `{"status":"ready"}`. On failure it returns `503` with `{"status":"not_ready","error":"..."}`.
