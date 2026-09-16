# Troubleshooting

Start with the status code or error text shown by the client. Each entry explains
what the error means, what to try first, how to confirm recovery, and what to
capture if it keeps happening.

## `400 provider_state_unavailable`: provider-state ownership is unavailable

On an explicit model route, this code means one or more supplied provider-state
values have no ownership binding in the active store. It applies both when
all bindings are missing and when some remain known. The request is rejected
before any provider send, including when streaming was requested. HTTP responses
retain `type: invalid_request_error`; WebSocket error frames carry the same code
and `status_code: 400`. The compact and memory shims also report it when their
state validation encounters a missing binding.

The code does not identify why proof is missing. A binding may never have been
observed here, or may have been lost through expiry, eviction, restart, or sending
the request to another process in default memory-only mode. With opt-in
[durable ownership](state-recovery.md), proof survives process restart in the
same store but may be missing because it was never committed there, was explicitly
pruned, or the original store was lost or replaced. A proven owner disagreement, conflict tombstone,
malformed value, or cross-route binding still fails with its existing validation
error, even when other bindings are missing. A known owner that disagrees with an
established WebSocket target pin also remains a conflict.

If the original Vekil process still holds memory bindings, restore request affinity
to that process and keep the configured owner stable. For durable mode, reopen the
original intact store with the same owner configuration and authenticated account;
only one process can use it at a time. Otherwise Vekil cannot
reconstruct ownership from opaque state. Retrying or compacting the same missing
state does not recover it. Preserve the transcript; begin a fresh session with
client-supported context if continuation cannot be restored. Do not strip state
or switch providers to bypass this check. Enabling durable mode after proof has
already been lost does not reconstruct it or permit cross-target replay.

## `429`: upstream rate limit

Keep the `Retry-After` response header. Vekil preserves long resets and returns
the upstream response immediately when another attempt cannot fit within the
request timeout. It also honors `retry-after-ms` and exhausted quota reset
headers. Increasing the timeout does not increase the upstream quota.

For recognized Copilot limits with a valid reset, Vekil shares a process-local
cooldown across affected requests. Model limits apply to that model and
credential, account and weekly limits apply across models for that credential,
and integration limits apply to that integration within the configured provider.
An active cooldown returns 429 without sending another inference request. After
the reset, one request probes availability before queued callers continue.
Cooldown records are bounded and lost on restart.

The same rules apply when a Chat, native Messages, or Responses stream returns a
structured throttle after HTTP `200`. This includes Responses-backed Chat and
requests streamed internally for tool-call aggregation. Responses errors can
supply reset headers in the stream itself.
A recovery probe that repeats the throttle renews the cooldown before queued
requests continue. An already-started client stream reports the error in its
stream; a non-streaming request returns `429`.

Configured priority failover can still use an unaffected compatible target.
Requests with provider-bound state retain their selected target. Switching models
within the same account does not avoid an account or weekly cooldown.

Requests waiting for a recovery probe honor disconnects, request deadlines, and
shutdown. The number of waiters is bounded; excess requests receive 503.

To investigate repeated throttling, record the error code, reset, timestamp, and
`X-Copilot-Service-Request-Id` when present. Quota snapshot and usage-rate-limit
headers provide additional account-specific diagnostics. Avoid sharing request
contents or credentials.

## `408 user_request_timeout`: timed out reading request body

### Symptom

Codex may report:

```text
unexpected status 408 Request Timeout: upstream error (408):
Timed out reading request body. Try again, or use a smaller request size.
(code=user_request_timeout)
```

The URL in the error is the local Vekil endpoint because Codex connected to
Vekil. The nested `upstream error` means the selected provider returned the 408
while reading the forwarded body.

This is separate from Vekil's `64 MiB` inbound limit for Responses requests.
Changing `--streaming-upstream-timeout` does not address it because response
streaming has not started.

### What to do first

Retry the turn. Vekil treats HTTP 408 as transient and sends a fresh request body
within its retry budget. The client may also retry after Vekil returns the final
error.

If the same session keeps failing, compact it or start a new session with less
history. If several independent sessions fail in the same time window, the
selected provider's ingress path may be having trouble accepting request bodies.

### How to confirm recovery

For a launcher session, inspect the proxy JSON log path printed in the startup
banner. For a normal server, use the dashboard or `GET /stats.json`:

- `retries_by_code` with label `408` means Vekil retried an upstream 408.
- `status_codes` with label `408` counts requests that still ended with 408.
- A newer `recent` entry for the same endpoint and model with `"status": 200`
  confirms that a later request completed.

### What to capture if it repeats

Keep the failed request ID and timestamp. Also record the public model or route,
whether one or several sessions were affected, and the relevant proxy log lines.
These details distinguish one large session replay from a provider-wide
ingress problem without exposing the request body.
