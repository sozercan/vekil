# 0002: Preserve distinct Anthropic wire shapes

Status: Accepted

## Context

Vekil's `models` package holds data-only representations. The Anthropic Messages wire format uses union-shaped fields and distinguishes absent fields from some empty values. Using a single more narrowly typed Go representation can alter a valid request or response before translation.

## Decision

- Keep `AnthropicMessage.Content` as raw JSON so it can hold either a string or an array of typed content blocks. Validate and translate those shapes in `proxy`, not in the model struct.
- Keep `ContentBlock.Thinking` as a string pointer. Anthropic clients may read `thinking` unconditionally on a thinking block, so a present empty value must serialize as `"thinking":""`; a plain `string` with `omitempty` would omit it. A nil pointer still omits the field for unrelated block types.
- Carry `enabled`, `disabled`, and `adaptive` thinking modes separately from the optional enabled-mode budget. Preserve the `auto`, `any`, and named `tool` tool-choice forms for the Anthropic translator to interpret.

## Consequences

The wire structs don't collapse these distinctions into a single internal meaning. Translation and tests own the supported subsets on each backend.
