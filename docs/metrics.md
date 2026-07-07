# Prometheus Metrics

Vekil exposes a Prometheus-compatible `/metrics` endpoint for scraping by Prometheus, Grafana Agent, or any OpenMetrics-compatible collector. Metrics are enabled by default and can be disabled with `--no-metrics` or `NO_METRICS=true`.

## Metrics Reference

### Counters

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_requests_total` | `provider`, `public_model`, `endpoint`, `status` | Total requests processed |
| `vekil_tokens_total` | `provider`, `public_model`, `direction` | Total tokens (direction: `prompt` or `completion`) |
| `vekil_retries_total` | `provider`, `public_model`, `reason` | Upstream retry attempts (reason: `429`, `5xx`, `timeout`) |
| `vekil_upstream_errors_total` | `provider`, `public_model`, `code` | Upstream errors by HTTP status code |

### Histograms

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_request_duration_seconds` | `provider`, `public_model`, `endpoint` | Request latency (non-streaming only) |

**Default histogram buckets** (Prometheus client defaults): `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10`

These buckets cover the typical range of LLM inference latencies from 5ms to 10s. Streaming requests are excluded from duration observations because their wall-clock time is dominated by client hold time.

### Gauges

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_inflight_requests` | `provider` | Currently in-flight requests |
| `vekil_build_info` | `version`, `go_version`, `commit` | Always 1; useful for dashboard joins |

### Standard Go Runtime Metrics

The `/metrics` endpoint also exposes standard Go runtime metrics (`go_*` and `process_*` families) via the Prometheus client library's default collectors.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--no-metrics` | `NO_METRICS` | `false` | Disable the `/metrics` endpoint entirely |

When `--no-metrics` is set, no metrics collector is initialized and the `/metrics` route is not registered.

## Scraping

### Prometheus scrape config

```yaml
scrape_configs:
  - job_name: vekil
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:1337']
    metrics_path: /metrics
```

### Verify manually

```bash
curl http://localhost:1337/metrics
```

## Grafana Dashboard

An example Grafana dashboard JSON is provided at [`docs/grafana-dashboard.json`](grafana-dashboard.json). Import it into Grafana to get panels for:

- Request rate by provider/model
- Error rate and upstream error breakdown
- Latency histograms (p50, p95, p99)
- Token throughput (prompt vs completion)
- Retry rate by reason
- In-flight request gauge
- Build info annotation

## Label Notes

- `provider`: the provider ID from your providers config (e.g., `copilot`, `azure-prod`), or empty for requests that fail before routing resolves.
- `public_model`: the public model name as seen by the client (e.g., `claude-sonnet-4`, `gpt-4o`).
- `endpoint`: the internal endpoint type (`chat_completions`, `messages`, `responses`, `responses_ws`, or Gemini action names).
- `status`: HTTP status code as a string (e.g., `200`, `429`, `500`).
- `direction`: `prompt` or `completion` for token counters.
- `reason`: retry trigger category — `429` for rate limits, `5xx` for server errors, `timeout` for transport-level failures.
