# Development

## Build

```bash
go build -o copilot-proxy .
make build-app
make docker-build
```

`go test ./...` and ordinary Go builds do not require Sparkle. The updater code is only compiled for the packaged macOS app build via `make build-app`, which passes the `sparkle` build tag and embeds the framework.

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

## Manual Live Smoke Workflow

The repository also includes a manual `Live Copilot Smoke` workflow in [`.github/workflows/live-copilot-smoke.yaml`](../.github/workflows/live-copilot-smoke.yaml).

It builds the proxy, installs Codex, Claude Code, and Gemini CLI on a GitHub-hosted runner, and then runs [`scripts/live-cli-smoke.sh`](../scripts/live-cli-smoke.sh).

The smoke script starts the proxy with a non-interactive GitHub token, waits for `/readyz`, selects currently available OpenAI, Anthropic, and Gemini models from `/v1/models`, and runs one file-reading headless check per CLI using isolated temp-home config directories.

To use it:

1. Create a GitHub token for a user that has GitHub Copilot access.
2. Grant that token the `Copilot Requests` permission.
3. Save it as the repository secret `COPILOT_GITHUB_TOKEN`.
4. Run the `Live Copilot Smoke` workflow from the Actions tab.

This workflow is intentionally separate from the normal CI workflow so pull requests and forked builds remain deterministic and do not depend on Copilot credentials.

You can also run the same smoke script locally after building `copilot-proxy` and installing those three CLIs.
