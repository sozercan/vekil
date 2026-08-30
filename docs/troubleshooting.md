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

## Container client gets connection refused or cannot resolve `host.docker.internal`

### Symptom

An agent running in a container cannot connect to a Vekil process running on
the host. Requests may fail with `connection refused`, `could not resolve host`,
or a similar name-resolution error even though the proxy works at
`http://localhost:1337` on the host.

### What to do first

Confirm that Vekil is bound to the Docker bridge gateway, not the default
`127.0.0.1`. On Linux or WSL2, also add the
`host.docker.internal:host-gateway` mapping with Compose `extra_hosts` or the
`docker run --add-host` option. Docker Desktop supplies this hostname on macOS
and Windows.

Follow [Reach a host-run Vekil from a container](clients.md#reach-a-host-run-vekil-from-a-container)
for the gateway-discovery, bind, and client configuration examples.

### How to confirm recovery

From the same container and network as the agent, run:

```bash
curl http://host.docker.internal:1337/healthz
```

A successful health response confirms both hostname resolution and network
reachability. Then retry the client with its base URL set to the same host and
port.

### What to capture if it repeats

Record the host operating system, Docker or Docker Desktop version, container
network name, the bridge gateway reported by `docker network inspect bridge`,
the effective Vekil listen address, and the exact DNS or connection error from
inside the container. Do not include provider credentials.
