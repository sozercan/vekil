# Speed-Tier Routing

Speed-tier routing is an optional provider-config feature that can route a request for a public model to a configured cheaper or faster sibling before the upstream request is sent. It is disabled by default and is deliberately conservative: Vekil only uses strict request-shape signals, never an LLM judge or quality score.

The contract is narrow: the request matched an operator-defined shape where the operator prefers the configured speed/cost tier. Vekil does **not** claim that the target model is equivalent to the requested model, and it does not retry against the original model when the downgrade was a bad choice.

## Configuration

Enable the feature with the top-level kill switch, then opt in individual source models with `models[].speed_tier`:

```yaml
speed_tier_enabled: true
providers:
  - id: anthropic
    type: anthropic-compatible
    base_url: https://api.anthropic.example.com
    auth_type: bearer
    api_key_env: ANTHROPIC_API_KEY
    models:
      - public_id: claude-sonnet-4.5
        deployment: claude-sonnet-4-5-20250929
        endpoints: [/v1/messages, /chat/completions]
        speed_tier:
          downgrade_to: claude-haiku-4.5
          semantics: all
          ws_sticky_after_upgrade: true
          when:
            max_tokens_lte: 512
            tools_count_lte: 0
            input_chars_lte: 16000
            system_chars_lte: 2048
            message_count_lte: 3
            require_endpoint_in: [/v1/messages]
          never_when:
            thinking_enabled: true
            reasoning_effort_in: [medium, high]
            has_header: X-Vekil-No-Downgrade

      - public_id: claude-haiku-4.5
        deployment: claude-haiku-4-5-20251001
        endpoints: [/v1/messages, /chat/completions]
```

`speed_tier_enabled` defaults to `false`, so adding a `speed_tier` block is inert until the operator enables the global switch.

## Validation Rules

Startup fails when a configured speed-tier pair is unsafe:

- `downgrade_to` must reference another `public_id` in the same provider.
- The target must support every endpoint the source supports.
- The target must not declare its own `speed_tier`; chained downgrades are rejected.
- `semantics` must be `all` or `any`. Empty means `all`.

These checks run while static provider models are built, before traffic is accepted.

## Request Signals

Signals are computed from the current request body and headers only:

| Field | Meaning |
|---|---|
| `max_tokens_lte` | Match when `max_tokens`, `max_completion_tokens`, or `max_output_tokens` is present and below the threshold. |
| `tools_count_lte` | Match when top-level `tools` is present and its length is below the threshold. |
| `input_chars_lte` | Match when the raw JSON request body is below the threshold. This is a cheap proxy, not tokenization. |
| `system_chars_lte` | Match when detected system/instruction text is below the threshold. |
| `message_count_lte` | Match when top-level `messages` or array `input` length is below the threshold. |
| `require_endpoint_in` | Match only on listed provider endpoints such as `/v1/messages`, `/chat/completions`, or `/responses`. |

`semantics: all` requires every configured `when` signal to match. `semantics: any` downgrades when at least one configured `when` signal matches. Missing request fields are safe: field-dependent signals do not match when the field is absent.

## Denylists and Escape Hatches

`never_when` always wins over `when` and explicit speed requests:

```yaml
never_when:
  thinking_enabled: true
  reasoning_effort_in: [medium, high]
  has_header: X-Vekil-No-Downgrade
```

Built-in client escape hatches:

- `X-Vekil-Routing: no-downgrade` or `X-Vekil-Routing: default` disables downgrade for the request.
- `X-Vekil-No-Downgrade: 1` disables downgrade for the request.
- `X-Vekil-Routing: speed` or legacy `X-Vekil-Tier: speed` explicitly requests the configured speed tier when no denylist signal fires.
- `fast/<model>` is a model alias for explicit speed-tier routing. If the source model has no configured speed tier or the global switch is off, Vekil fails open to the source model.

## WebSocket Bridge Behavior

The Codex-style `GET /v1/responses` websocket bridge evaluates speed-tier routing per turn. A turn that starts as a small conversational request can downgrade, while a later tool-heavy or deeper turn can route to the original model.

When `ws_sticky_after_upgrade` is true (default), a websocket session that has downgraded and then grows out of the gate pins back to the source model for the rest of that session. This avoids mid-session model oscillation for clients that maintain long-lived state.

## Structured Logs

Every considered configured speed-tier decision emits an info log with fields such as:

| Field | Meaning |
|---|---|
| `from` | Source public model ID. |
| `to` / `routed_to` | Target public model ID when downgraded. |
| `decision` | `downgraded`, `forced_alias`, `considered_rejected`, or `opted_out`. |
| `triggering_signal` | First matching signal for `any`, `all` for all-mode, `client_header`, or `fast_alias`. |
| `input_chars`, `system_chars`, `tools_count`, `message_count`, `max_tokens` | Computed request-shape features. |
| `endpoint` | Provider endpoint used for routing. |
| `client_opt_out` | Whether a client opt-out header fired. |
| `reason` | Rejection or denylist reason when not downgraded. |

The log intentionally uses `routed_to`, not `equivalent_to`; it records routing, not quality equivalence.

## Recommended Starting Points

These are examples only. Leave them commented until you have checked your own latency/cost and quality tolerance:

```yaml
# speed_tier_enabled: true
# providers:
#   - id: anthropic
#     models:
#       - public_id: claude-sonnet-4.5
#         speed_tier:
#           downgrade_to: claude-haiku-4.5
#           semantics: all
#           when:
#             max_tokens_lte: 512
#             tools_count_lte: 0
#             input_chars_lte: 16000
#           never_when:
#             thinking_enabled: true
#             has_header: X-Vekil-No-Downgrade
#       - public_id: claude-haiku-4.5
#
#   - id: gemini
#     models:
#       - public_id: gemini-2.5-pro
#         speed_tier:
#           downgrade_to: gemini-2.5-flash
#           semantics: all
#           when:
#             max_tokens_lte: 512
#             tools_count_lte: 0
#             input_chars_lte: 16000
#       - public_id: gemini-2.5-flash
```

Do not assume a cheaper model is faster for every provider/model pair. If cost is the only goal, configure that explicitly and monitor client outcomes.

## Honest Unknowns

Vekil cannot know inline that a downgrade was wrong. Weak signals such as client retries, user follow-ups, or later tool failures are delayed and ambiguous; replaying against the original model would double latency and cost. Operators should use the global kill switch, per-request opt-outs, and structured decision logs to audit the policy against user feedback.
