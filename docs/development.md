# Development

## Build

```bash
go build -o copilot-proxy .
make build-app
make docker-build
```

## Test

```bash
go test ./... -count=1
go test ./proxy/ -run TestHandle -v
go test ./proxy/ -run TestMapStopReason/stop -v
```

## Benchmarks

```bash
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesWebSocketRequestBuild' -benchmem -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesTransport' -benchmem -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesSession' -benchmem -count=1
```

## Lint

```bash
go vet ./...
make lint
```

## CI

GitHub Actions in `.github/workflows/ci.yaml` runs lint, tests, build, vet, and e2e validation before merge.
