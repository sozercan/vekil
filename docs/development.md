# Development

## Build

```bash
go build -o vekil .
make build-app
make docker-build
```

`go test ./...` and ordinary Go builds do not require Sparkle. The updater code is only compiled for the packaged macOS app build via `make build-app`, which downloads Sparkle 2.9.0 into `.build/sparkle/`, passes the `sparkle` build tag, embeds `Sparkle.framework`, and ad-hoc signs the finished app bundle.

## Test

```bash
go test ./... -count=1
make test-app          # macOS only; builds and verifies Vekil.app
scripts/macos-app-smoke.sh  # macOS only; build + launch smoke for Vekil.app
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

GitHub Actions in [`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml) runs lint, tests, build, vet, and e2e validation before merge.

The macOS menubar app has its own workflow in [`.github/workflows/macos-app.yaml`](../.github/workflows/macos-app.yaml). It runs `scripts/macos-app-smoke.sh` on a macOS runner, which builds `Vekil.app`, validates the bundle contents, launches the app through Launch Services, verifies it stays up, and then quits it cleanly.

## Release

Tag pushes to [`.github/workflows/release.yaml`](../.github/workflows/release.yaml) now use [`.goreleaser.yaml`](../.goreleaser.yaml) to publish the CLI binaries and checksums to GitHub Releases.

The same release workflow also:

- builds `vekil-macos-arm64.zip` on a macOS runner and uploads it to the tagged release
- generates and uploads `appcast.xml` for Sparkle update checks
- updates the `vekil` cask in `sozercan/homebrew-repo`
- pushes the multi-arch container image to GHCR

To publish the Homebrew cask, configure the repository secret `HOMEBREW_REPO_TOKEN` with push access to `sozercan/homebrew-repo`.

To publish Sparkle updates, configure both `SPARKLE_PUBLIC_ED_KEY` and `SPARKLE_PRIVATE_ED_KEY` in the repository secrets.

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

You can also run the same smoke script locally after building `vekil` and installing those three CLIs.
