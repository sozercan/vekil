# Gemini Compatibility

Gemini endpoints are implemented as a translation layer, not zero-copy passthrough. Requests are translated to OpenAI Chat Completions, routed through the provider that owns the selected public model, and translated back into Gemini responses.

## `POST /v1beta/models/{model}:generateContent`, `POST /v1/models/{model}:generateContent`, and `POST /models/{model}:generateContent` (Gemini)

The proxy accepts all three Gemini route prefixes: `/v1beta/models/{model}:...`, `/v1/models/{model}:...`, and `/models/{model}:...`.

The decoder accepts both standard Gemini camelCase fields and LiteLLM-style snake_case aliases such as `system_instruction`, `function_declarations`, `inline_data`, `max_output_tokens`, and `response_json_schema`.

Supported subset:

- `contents[].role`; omitted or empty roles default to `user`, while explicit roles must be `user` or `model`
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

Validation failures (`400 INVALID_ARGUMENT`) include path/body model mismatches, explicit content roles other than `user` or `model`, malformed content parts, invalid function-call history, and unmatched `functionResponse` parts.

`functionResponse.response` objects retain their JSON structure when translated for generic Gemini clients. For Gemini CLI requests, an exact one-field `{"output":"<text>"}` response is unwrapped to the text value; responses with metadata or non-string `output` values remain structured JSON.

## `POST /v1beta/models/{model}:streamGenerateContent`, `POST /v1/models/{model}:streamGenerateContent`, and `POST /models/{model}:streamGenerateContent` (Gemini)

`streamGenerateContent` uses the same request body, translation rules, and validation behavior as `generateContent`, but returns data-only SSE frames instead of a single JSON response.

Streaming behavior:

- each SSE `data:` frame contains a partial Gemini `GenerateContentResponse` payload
- text deltas are emitted as Gemini `candidates[].content.parts[].text` parts
- tool calls are buffered across the upstream turn and emitted as Gemini `functionCall` parts in numeric index order when the turn completes
- sparse upstream tool-call indices are ordered by their actual numeric keys without scanning missing indices; negative indices are rejected as malformed upstream data
- a final frame can include Gemini `finishReason` and `usageMetadata`

After the SSE response has committed HTTP `200`, an upstream rate-limit frame is translated to a Gemini error frame with code `429` and status `RESOURCE_EXHAUSTED`; overload is translated to code `503` and status `UNAVAILABLE`. A truncated stream that ends before `[DONE]` produces code `502` and status `UNAVAILABLE`. The outer HTTP status remains the already-committed `200` in all three cases. When a non-streaming `generateContent` request is force-streamed upstream for tool-call reliability, the same failures are returned before the client response commits, so the HTTP status is respectively `429`, `503`, or `502`.

Use `curl -N` or another SSE-capable client so streamed frames are not buffered locally.

## `POST /v1beta/models/{model}:countTokens`, `POST /v1/models/{model}:countTokens`, and `POST /models/{model}:countTokens` (Gemini)

`countTokens` uses the same accepted route prefixes, omitted-role default, and content validation as the other Gemini compatibility routes. It normalizes the Gemini request into the same prompt/tool payload used by `generateContent`, performs a minimal upstream `/chat/completions` probe, and returns `usage.prompt_tokens` as Gemini `totalTokens`. Normalized successful requests are cached for 60 seconds; expired entries are pruned globally and the cache is capped at 1,024 request hashes with deterministic oldest-entry eviction. If the probe hits a transient transport error, 429, 5xx, or a 200 response with missing usage, the proxy returns a dependency-free local token estimate instead of failing the counting request. It still surfaces permanent client/configuration failures such as 400, 401, and 403 without estimating.

### Function calling modes

Gemini `functionCallingConfig.mode: "NONE"` is translated to OpenAI `tool_choice: "none"`. For non-streaming `generateContent` requests, this lets the proxy use a non-streaming upstream request even when tool declarations are present, because the selected mode forbids tool calls.

Gemini `functionCallingConfig.mode: "VALIDATED"` is accepted and translated to OpenAI `tool_choice: "auto"`. OpenAI Chat Completions has no equivalent schema-validation-guarantee tier, so this is a lossy compatibility mapping: the model may decide whether to call a tool, but any Gemini-specific validation guarantee is not preserved by the OpenAI-shaped upstream request.
