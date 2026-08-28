# Synthetic Chat-over-Responses fixtures

These fixtures model only the sanitized protocol structures observed during Phase 0. They contain no copied prompts, tool output, identifiers, account data, authorization data, or replayable encrypted content. Every model, ID, message, tool name, argument, and encrypted-content value is deliberately synthetic.

## Fixture inventory

| Scenario | Non-streaming Responses JSON | Responses SSE |
|---|---|---|
| Text | `nonstream_text.json` | `stream_text.sse` |
| One function call | `nonstream_one_tool_call.json` | `stream_one_tool_call.sse` |
| Parallel calls / interleaved deltas | `nonstream_parallel_tool_calls.json` | `stream_parallel_interleaved_tool_calls.sse` |
| Reasoning followed by a final message | `nonstream_reasoning_message_continuation.json` | `stream_reasoning_message_continuation.sse` |
| Reasoning, then text, then a function call | — | `stream_reasoning_tool_call.sse` |
| Immediate failure | `nonstream_immediate_failure.json` | `stream_immediate_failure.sse` |
| Output-token incomplete (`length`) | `nonstream_incomplete_length.json` | `stream_incomplete_length.sse` |
| Unsupported/malformed semantic type | `nonstream_malformed_unknown_item.json` | `stream_malformed_unknown_event.sse` |

The last pair is intentionally invalid at the protocol-semantic level while retaining valid JSON and SSE framing. It is for deterministic fail-closed tests. These are upstream Responses fixtures, so successful streams end with a Responses terminal event rather than Chat's `[DONE]` marker.

## Sanitized Phase 0 observations

- Claude Code 2.1.210 produced translated Chat requests containing only `model`, `messages`, `max_tokens` (`32000`), `stream` (`true`), `stream_options.include_usage` (`true`), `tools`, and `parallel_tool_calls` (`true`).
- A single tool result was a string. Two parallel tool results were returned together and in the original call order.
- Non-streaming Responses text used a `message` item with `output_text`. Function calls used `function_call` items with distinct `id` and `call_id` values plus `name` and string `arguments`.
- A continuation response contained a `reasoning` item followed by a `message`; reasoning could include `encrypted_content`, and the final message used `phase: final_answer`.
- Observed streams used `response.created`, `response.in_progress`, `response.output_item.added`, `response.output_item.done`, `response.content_part.added`, `response.content_part.done`, `response.output_text.delta`, `response.output_text.done`, `response.function_call_arguments.delta`, `response.function_call_arguments.done`, and `response.completed`. Parallel argument deltas may interleave. The terminal `response.completed.response.output` was the authoritative complete output.
- Copilot's opaque Responses item `id` / event `item_id` values changed between events for the same output item. Stable correlation came from `output_index` (and `content_index` for message parts); function `call_id` remained stable. The adapter therefore never exposes or authorizes replay by those opaque event item IDs.
- Copilot also changed the opaque response `id` between `response.created` and the terminal response event on a single contiguous stream. The adapter requires both IDs to be nonempty but does not require equality; stream sequence/state and output indexes provide correlation.
- Exact prior-output replay and stateless synthetic historical `function_call` replay both succeeded.
- Full parallel outputs succeeded in any result order. Replaying the complete prior call group with only a partial output was rejected; replaying only the matching call with that output succeeded and reissued the missing call.
- Claude Code accepted IDs shaped as `call_vekil_<22-character-base64url>` and returned both parallel results together.

## Final Phase 0 request-field matrix

| Status | Chat fields / values |
|---|---|
| **MAP** | `model`, `messages`, `stream`, `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, function `tools`, `tool_choice`, `parallel_tool_calls`, `response_format`, `reasoning_effort`, `verbosity`, `metadata`, `store`, `user`, `prompt_cache_key`, `safety_identifier` |
| **LOCAL** | `stream_options.include_usage` |
| **REJECT** | `service_tier`; non-empty `stop`; `n` other than `1`; `frequency_penalty`, `presence_penalty`, or other penalties; `seed`; `logit_bias`; `logprobs`/`top_logprobs`; `audio`; `modalities`; `prediction`; non-function tools; unknown top-level fields; `messages[].name` |

`max_tokens` and `max_completion_tokens` both map to `max_output_tokens`; when both are present they must agree, and public Responses-backed limits from 1 through 15 are rejected rather than clamped. Omitted `n` or `n: 1` is accepted. Only function tools are in the mapped subset. No Responses-backed field is assigned `EXISTING-IGNORE`; unsupported input must fail explicitly rather than be dropped.

Omitted or null `store` is materialized as `false`, and Vekil injects `include: ["reasoning.encrypted_content"]` internally for storage-disabled replay.
