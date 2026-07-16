# Prometheus Metrics

Vekil exposes a Prometheus-compatible `/metrics` endpoint for scraping by Prometheus, Grafana Agent, or any OpenMetrics-compatible collector. Metrics are enabled by default and can be disabled with `--no-metrics` or `NO_METRICS=true`.

## Metrics Reference

### Counters

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_requests_total` | `provider`, `public_model`, `endpoint`, `status` | Total requests processed |
| `vekil_tokens_total` | `provider`, `public_model`, `direction` | Total tokens (direction: `prompt` or `completion`) |
| `vekil_retries_total` | `provider`, `public_model`, `reason` | Upstream retry attempts (reason: `429`, `5xx`, `timeout`, `other`) |
| `vekil_upstream_errors_total` | `provider`, `public_model`, `code` | Failed requests that reached an upstream attempt, by final HTTP status code |

### Histograms

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_request_duration_seconds` | `provider`, `public_model`, `endpoint` | Request latency (non-streaming only) |

**Histogram buckets (seconds):** `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300`

The extended buckets preserve useful p95/p99 resolution for non-streaming LLM calls that take tens of seconds or several minutes. Streaming requests are excluded from duration observations because their wall-clock time is dominated by client hold time.

### Gauges

| Metric | Labels | Description |
|--------|--------|-------------|
| `vekil_inflight_requests` | — | Currently in-flight requests (global) |
| `vekil_build_info` | `version`, `go_version`, `commit` | Always 1; useful for dashboard joins |

### Standard Go Runtime Metrics

The `/metrics` endpoint also exposes standard Go runtime metrics (`go_*` and `process_*` families) via the Prometheus client library's default collectors.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--no-metrics` | `NO_METRICS` | `false` | Disable the `/metrics` endpoint entirely |

When `--no-metrics` is set, no metrics collector is initialized and the `/metrics` route is not registered. Otherwise, `/metrics` remains scrapeable while startup authentication or dynamic provider validation is pending so runtime and build diagnostics stay available.

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
- In-flight request gauge (global)
- Build info annotation

## Label Notes

- `provider`: the provider ID from your providers config (e.g., `copilot`, `azure-prod`), or empty for requests that fail before routing resolves.
- `public_model`: the public model name as seen by the client (e.g., `claude-sonnet-4`, `gpt-4o`). Values are length-bounded, and after 200 distinct model labels per process, new values fold into `other` to protect Prometheus cardinality.
- `endpoint`: the internal endpoint type emitted by tracked routes: `openai_chat`, `anthropic`, `gemini`, `responses`, or `responses_ws`.
- `status`: HTTP status code as a string (e.g., `200`, `429`, `500`).
- `direction`: `prompt` or `completion` for token counters.
- `reason`: retry trigger category — `429` for rate limits, `5xx` for server errors, `timeout` for transport-level failures, `other` for any unexpected status.

## Error Attribution

`vekil_requests_total` records every tracked request outcome, including local validation failures. `vekil_upstream_errors_total` is narrower: it increments only when Vekil actually started an upstream HTTP attempt and the final request status is an error. This keeps malformed local requests and routing-validation failures out of upstream-health alerts while retaining transport failures and upstream error responses.
