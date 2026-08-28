# Troubleshooting

Start with the status code or error text shown by the client. Each entry explains
what the error means, what to try first, how to confirm recovery, and what to
capture if it keeps happening.

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
