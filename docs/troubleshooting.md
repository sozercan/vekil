# Troubleshooting

Start with the status code or error text shown by the client. Each entry explains
what the error means, what to try first, how to confirm recovery, and what to
capture if it keeps happening.

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

The same rules apply when a Chat or native Messages stream returns a structured
throttle after HTTP `200`, including Chat requests streamed internally for
tool-call aggregation.
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
