# Prometheus Metrics

Vekil exposes a Prometheus-compatible `/metrics` endpoint for scrape-based observability. The endpoint is enabled by default and can be disabled with `--metrics=false` or `METRICS=false`.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--metrics` | `METRICS` | `true` | Enable Prometheus `/metrics` endpoint |

When disabled, the `/metrics` route is not registered and no metric collection overhead is incurred.

## Scrape Configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: vekil
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:1337']
    metrics_path: /metrics
```

## Exposed Metrics

### Request Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vekil_requests_total` | Counter | `provider`, `public_model`, `endpoint`, `status` | Total proxy requests |
| `vekil_request_duration_seconds` | Histogram | `provider`, `public_model`, `endpoint` | End-to-end request duration (observed on stream close for streaming) |
| `vekil_first_byte_duration_seconds` | Histogram | `provider`, `public_model`, `endpoint` | Time to first byte for streaming requests |

### Token Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vekil_tokens_total` | Counter | `provider`, `public_model`, `direction` | Total tokens processed (`direction` is `prompt` or `completion`) |

### Retry and Error Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vekil_retries_total` | Counter | `provider`, `public_model`, `reason` | Upstream retry attempts (`reason` is `429`, `5xx`, or `timeout`) |
| `vekil_upstream_errors_total` | Counter | `provider`, `public_model`, `code` | Upstream error responses by HTTP status code |

### Operational Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vekil_inflight_requests` | Gauge | `provider` | Currently in-flight requests per provider |
| `vekil_endpoint_healthy` | Gauge | `provider`, `endpoint` | Endpoint health (0/1); populated when multi-endpoint selector is active |
| `vekil_build_info` | Gauge | `version`, `go_version`, `commit` | Build information; always 1 |

### Go Runtime Metrics

Standard Go runtime and process metrics from `prometheus/client_golang`:

- `go_goroutines`, `go_threads`, `go_gc_duration_seconds`, `go_memstats_*`
- `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, etc.

## Histogram Buckets

Both `vekil_request_duration_seconds` and `vekil_first_byte_duration_seconds` use the Prometheus default buckets:

```
.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10
```

These cover the typical range of LLM inference latencies from fast cached responses (~5ms) through long-running completions (~10s+).

## Label Cardinality

The `public_model` label is client-controlled. To prevent unbounded Prometheus series from arbitrary model strings:

- Model names longer than 64 characters are truncated.
- The same cardinality bounding used by the internal dashboard stats applies.

Provider and endpoint labels are server-configured and trusted.

## Build Info

`vekil_build_info` is set from the same `-ldflags` used by `vekil --version`:

```bash
# Check version
vekil --version
# Output: vekil 1.0.0 (commit: abc1234, go: go1.25.0)
```

The Makefile and Dockerfile inject `-X main.version` and `-X main.commit` at build time.

## Example Queries

```promql
# Request rate by provider (last 5m)
rate(vekil_requests_total[5m])

# P99 request duration by model
histogram_quantile(0.99, rate(vekil_request_duration_seconds_bucket[5m]))

# Token throughput
rate(vekil_tokens_total[5m])

# Retry rate by reason
rate(vekil_retries_total[5m])

# Error rate by provider
rate(vekil_upstream_errors_total[5m])
```
