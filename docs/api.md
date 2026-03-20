# API Reference

## `POST /v1/messages` (Anthropic)

Anthropic Messages compatibility for the supported content and tool subset. Requests are translated to OpenAI Chat Completions, forwarded upstream, and translated back to Anthropic.

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

### `GET /v1/models`

The live model list is fetched dynamically from the upstream Copilot API. The proxy preserves the upstream OpenAI-style `data` payload and also adds a Codex-compatible top-level `models` array.

```bash
curl http://localhost:1337/v1/models
```

Example IDs in recent upstream responses have included:

- `gpt-4o`
- `gpt-4.1`
- `gpt-5-mini`
- `gpt-5.1`
- `gpt-5.1-codex`
- `gpt-5.1-codex-mini`
- `gpt-5.1-codex-max`
- `gpt-5.2`
- `gpt-5.2-codex`
- `gpt-5.3-codex`
- `gpt-5.4`
- `claude-haiku-4.5`
- `claude-sonnet-4`
- `claude-sonnet-4.5`
- `claude-sonnet-4.6`
- `claude-opus-4.5`
- `claude-opus-4.6`
- `claude-opus-4.6-1m`
- `gemini-2.5-pro`
- `gemini-3-pro-preview`
- `gemini-3-flash-preview`
- `gemini-3.1-pro-preview`

## `POST /v1beta/models/{model}:generateContent` and `POST /models/{model}:generateContent` (Gemini)

Gemini is implemented as a translation layer, not a zero-copy passthrough layer. Gemini requests are translated to OpenAI Chat Completions, sent upstream, and translated back into Gemini responses.

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

## `POST /v1/responses` and `GET /v1/responses` (OpenAI)

`POST /v1/responses` is a near zero-copy passthrough for the OpenAI Responses API. Proxy-owned synthetic compaction items are expanded back into normal context before forwarding so resumed Codex sessions continue through the standard `/v1/responses` path.

Streaming Responses requests are passed through directly with upstream headers preserved.

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

See [configuration.md](configuration.md) for tuning flags.

## `POST /v1/responses/compact`

Compatibility shim for environments expecting `/responses/compact`. The proxy rewrites the request into a normal upstream `/responses` call with a compaction prompt, then returns:

- a normal assistant summary message
- a proxy-owned opaque `compaction` item whose `encrypted_content` can later be sent back to `/v1/responses` or `/v1/responses/compact`

If the requested model does not support the upstream Responses API, the proxy retries against a compatible fallback model discovered from `/models`.

## `POST /v1/memories/trace_summarize`

Compatibility shim that summarizes one or more traces into `{trace_summary, memory_summary}` objects using the upstream `/responses` endpoint plus a JSON-only summarization prompt.

## `GET /healthz`

Returns `{"status":"ok"}`.

## `GET /readyz`

Validates that the proxy can obtain a Copilot token and successfully probe upstream `/models`. On success it returns `{"status":"ready"}`. On failure it returns `503` with `{"status":"not_ready","error":"..."}`.
