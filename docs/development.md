# Development

## Build

```bash
go build -o vekil .
make build-app
make docker-build
make docker-build-rtk   # optional RTK image variant
make docker-rtk-e2e     # verifies bundled RTK reduces tool output through Vekil
```

`go test ./...` and ordinary Go builds do not require Sparkle. The updater code is only compiled for the packaged macOS app build via `make build-app`, which downloads Sparkle 2.9.0 into `.build/sparkle/`, passes the `sparkle` build tag, embeds `Sparkle.framework`, and ad-hoc signs the finished app bundle.

## Test

```bash
go test ./... -count=1
make compaction-lab    # fast in-process compaction regression harness
make test-app          # macOS only; builds and verifies Vekil.app
scripts/macos-app-smoke.sh  # macOS only; build + launch smoke for Vekil.app
go test ./proxy/ -run TestHandle -v
go test ./proxy/ -run TestMapStopReason/stop -v
scripts/tests/live-smoke-reliability-test.sh  # deterministic mock-server/fake-CLI gates
scripts/tests/live-chat-over-responses-smoke-test.sh  # deterministic Chat-over-Responses live-harness gates
```

`cmd/compaction-lab` starts an in-process proxy and fake `/responses` upstream, then exercises the compact-response shape, opaque compaction replay, remote compaction v2 trigger handling, and websocket `response.processed` control frames. It is intended as a quick deterministic check for compaction regressions before running live Copilot smoke tests.

### Model-route safety suite

Provider-agnostic model-route implementation started from repository baseline `3388071`. Verify that the baseline is reachable from the revision under test before comparing behavior or performance:

```bash
git merge-base --is-ancestor 3388071 HEAD
printf 'baseline=%s head=%s\n' "$(git rev-parse 3388071)" "$(git rev-parse HEAD)"
```

The production-phase gate is:

```bash
make test
make vet
make build
go test -race ./... -count=1
```

For a fast route-focused pass before the full suite:

```bash
go test ./proxy/ -run 'Test(LoadProvidersConfigFile|ValidateModelRoutes|RouteReferenced|UnreferencedStatic|CompileExplicit|ExplicitRoute|ConfiguredExplicitRoute|StateBindingStore|RouteObservability|RequestSummary)' -count=1
```

Route-specific deterministic tests use local upstream servers plus injected transports/clocks; live provider credentials are not the merge gate. Coverage should include:

- strict version-1/version-2/version-3 decoding, duplicate-key rejection, limits, feature-matrix rejection, two-pass provider/route/policy references, normalized public-ID collisions, and offline `vekil config validate`;
- legacy route catalog/unknown-model/retry compatibility and explicit route ordering/catalog identity;
- pristine request body/header/auth construction for every target, including API-key, Entra, bearer, and custom-header switches;
- exact target-attempt and network-send counts for normal attempts, redirects, prewrite failures, ambiguous delivery, protocol recovery, compaction/replay, and compatibility fallback;
- Responses preamble commitment, partial/malformed streams, forced-stream semantic/tool progress, translated Anthropic/Gemini preambles, and client-write races;
- exact state binding before exposure, known/unknown/conflicting state, expiry/eviction/restart/second-process behavior, and pinned WebSocket first-turn/later-turn behavior with no cross-target migration;
- client-request versus physical-attempt accounting, final target attribution, bounded-cardinality/redaction rules, and failed-attempt usage isolation; and
- client disconnect, total deadline, shutdown, response-body cleanup, goroutine termination, and no-overlap/no-new-attempt races.

For large request and replay paths, keep the `64 MiB` request boundary in the deterministic matrix and verify that operation/send budgets prevent compaction, recovery, or fallback from creating an unbounded tree.

### Policy-routing safety suite

Schema-v3 policy routing adds a pre-dispatch planner above native OpenAI Chat. The deterministic merge gate must use in-memory classifier adapters and local `httptest` providers; live credentials and provider availability are supplementary, never substitutes for local tests.

Coverage should include:

- schema-v3 route exposure, internal-route non-resolution/catalog exclusion, public-entry/operational-ID collisions, maximum profile count, field ranges, recursive-policy rejection, and schema-v3 field rejection in v1/v2;
- terminal contract intersection, native `/chat/completions`-only destination validation, dynamic/unsupported provider rejection, and classifier one-target/one-attempt/one-send enforcement;
- exact global/profile mode ceiling behavior, including `off` making zero preflight/classifier calls and observe never changing dispatch;
- bounded canonical facts, UTF-8 truncation, non-text rejection, tool-name-only forwarding, total request cap, and exclusion of credentials, auth headers, provider state, replay IDs, physical routing metadata, parameter schemas, and tool arguments;
- mandatory content-forwarding, trust-domain, cross-domain, non-storage, and retention acknowledgements;
- strict forced `emit_policy_signals` parsing, duplicate-key/extra-field/enum/integer/trailing-content rejection, abstention, and exhaustive deterministic mapper precedence;
- non-blocking per-profile plus global admission, no queue/backlog, partial-admission release, per-profile fairness, cancellation before terminal dispatch, and shutdown cleanup;
- unavailable versus uncertain fallback separation, no fallback caching, infrastructure-only breaker transitions, timeout/content-output immunity, `Retry-After`, cooldown, and one half-open probe;
- sealed operation-plan immutability, classifier/terminal budget separation, exact selected-route sends, no cross-tier fallback, and preservation of forced-stream/aggregation behavior for both tiers;
- normalized public policy identity in Chat JSON/SSE, safe headers, errors, and metrics, with no terminal provider/route/target/deployment leakage; and
- adversarial prompt injection, malformed output, saturation, privacy, and cross-request isolation.

Run the full production gate under the race detector:

```bash
make test
make vet
make lint
make build
go test -race ./... -count=1
```

`vekil config validate` must remain offline. `vekil config validate --live` is an explicit operator smoke that uses a fixed non-user fixture to verify classifier auth/reachability, forced strict function output, non-storage request acceptance, and one physical send. Tests for that command should use controlled local providers so CI remains deterministic.

### Chat-over-Responses suite

For the Chat-over-Responses routing, conversion, replay, streaming, and public-ingress matrix, use:

```bash
go test ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1
go test -race ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1
go test ./proxy/ -run '^$' -fuzz 'FuzzTranslateChatRequestToResponses' -fuzztime=20s
```

The focused tests cover native-Chat preference, provider-local cold discovery, strict `MAP`/`LOCAL`/`REJECT` input handling, opaque replay IDs, full/reordered/partial parallel tool results, restart/state-loss errors, typed event streaming, Anthropic/Gemini ingress, count-token probes, and dashboard-compatible routing.

## Benchmarks

```bash
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesWebSocketRequestBuild' -benchmem -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesTransport' -benchmem -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesSession' -benchmem -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkChatOverResponses' -benchmem -count=3
```

The Chat-over-Responses benchmark regex includes permanent text-stream, fragmented function-argument, and interleaved parallel-tool cases. It also keeps the Phase 0 transport comparison available for regression work; review `ns/op`, `B/op`, and `allocs/op`, and do not introduce buffering proportional to the completed stream size.

Policy-routing benchmark evidence must compare native Chat request build, forced-stream aggregation, and transport before/after the planner change. Measure `off`, admitted/dropped `observe`, and synchronous `enforce` separately. The release gate is no measurable observe-mode p95 latency regression beyond bounded fact construction, and no more than 5% p95 proxy overhead beyond synchronous classifier time. Policy selection must not add terminal execution sends.

### Model-route benchmark baseline

For the model-route change, compare at least ten controlled samples against baseline `3388071`. Keep the Go version, fixtures, target count, and machine load identical, and set an explicit `GOMAXPROCS` instead of relying on the machine default:

```bash
export GOMAXPROCS=8  # choose one fixed value for both baseline and candidate
go version
go env GOOS GOARCH
printf 'GOMAXPROCS=%s\n' "$GOMAXPROCS"

go test ./proxy/ -run '^$' -bench 'BenchmarkChatRoute' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkDefaultProviderSetup' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkOpenAIResponseAggregatorToolCallArguments' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkGeminiStreamSparseToolCall' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesTransport' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesSession' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesWebSocketRequestBuild' -benchmem -count=10
go test ./proxy/ -run '^$' -bench 'BenchmarkExplicitRoutePreparedStreamTTFT|BenchmarkRouteAttemptStatsConcurrentContention|BenchmarkExplicitRouteTwoTargetFailover64MiB' -benchmem -count=10
```

Capture baseline and candidate output separately, then compare `ns/op`, `B/op`, and `allocs/op` with `benchstat`:

```bash
# Run the same benchmark command from a clean checkout/worktree at 3388071.
GOMAXPROCS="$GOMAXPROCS" go test ./proxy/ -run '^$' -bench '<same-regex>' -benchmem -count=10 > baseline-3388071.txt
GOMAXPROCS="$GOMAXPROCS" go test ./proxy/ -run '^$' -bench '<same-regex>' -benchmem -count=10 > candidate.txt
benchstat baseline-3388071.txt candidate.txt
```

Record the full baseline SHA, candidate SHA, Go version, OS/architecture, `GOMAXPROCS`, fixture/body size, stream mode, and target count with the results. Do not use the unrelated `fca0b12` commit as the route baseline; it is not an ancestor of `3388071` in this worktree. Credentialed Azure pool smoke is supplementary only and never replaces deterministic local tests or controlled benchmarks.

`BenchmarkChatRouteLegacyDirectResolutionRequestBuild` and `BenchmarkChatRouteExplicitPriorityOneTargetRequestBuild` provide the direct legacy-versus-route request-build baseline. `BenchmarkChatRouteLegacyDirectTransport` and `BenchmarkChatRouteExplicitPrimaryOnlyTransport` add deterministic `http.Client`/`RoundTripper` dispatch coverage without network variability. `BenchmarkExplicitRoutePreparedStreamTTFT` measures held-preamble handoff and reports `ttft-ns/op`; `BenchmarkRouteAttemptStatsConcurrentContention` measures concurrent physical-attempt accounting; and `BenchmarkExplicitRouteTwoTargetFailover64MiB` verifies exactly two sends and reports allocation pressure at the maximum request boundary. These checked-in benchmarks provide the scenarios, but the ten-sample baseline/candidate `benchstat` comparison remains release evidence that must be captured on a controlled machine rather than asserted from one local run.

## Policy evaluation and release evidence

Policy enforcement is an operator release gate, not an automatic consequence of merging the implementation. Keep the global ceiling `off` until all evaluation criteria in [Semantic Policy Routing](policy-routing.md#evaluation-gates-before-enforcement) pass.

At minimum, release evidence must include:

- separate development, pilot/calibration, untouched holdout, and adversarial datasets;
- at least 75 pilot tasks across always-lightweight, always-powerful, and the actual end-to-end policy path;
- a documented power analysis followed by at least three independent holdout executions per task/model/policy unless more are required;
- deterministic acceptance checks for objective coding tasks and blinded independent adjudication for subjective tasks;
- cost including classifier/terminal calls, retries, failures, and preflight amortization;
- at least 80% power at one-sided alpha `0.05`, a 2-point task-success non-inferiority margin, and a 0.5-point tool-validity margin versus always-powerful;
- at least 15% mean total cost improvement, at most 5% unavailable-plus-uncertain fallback, no extra terminal sends, and zero route/credential/cancellation/budget/identity invariant failures; and
- generation-attributable decisions plus observe sampling/admission-bias reporting.

Do not use holdout results to choose the classifier, modify its prompt/schema, or tune mapping thresholds. Do not splice multi-turn policy results from independent baseline trajectories; execute the real policy end to end.

After the release gate passes, require at least 5,000 completed observations per profile and 95% admission in every declared traffic bucket, then enforce one profile at a time with deployment-level 5% → 25% → 100% stateless Chat canaries. Roll back all profiles with `POLICY_ROUTING_MODE=off`. Keep direct stateful Responses/websocket traffic outside the policy canary pool or on its existing sticky topology.

## Lint

```bash
go vet ./...
make lint
```

## CI

GitHub Actions in [`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml) runs lint, tests, the full race detector, build, vet, a Kubernetes/kind operational smoke, and e2e validation before merge. Every core job has a job-level deadline. The test job runs [`scripts/tests/live-smoke-reliability-test.sh`](../scripts/tests/live-smoke-reliability-test.sh) with local mock servers and fake CLIs so stale-listener, timeout, per-client, and descendant-cleanup failures are deterministic, plus [`scripts/tests/live-provider-routing-smoke-test.sh`](../scripts/tests/live-provider-routing-smoke-test.sh), which builds the real binary and exercises schema-v2 two-target failover against controlled loopback Responses servers.

The kind smoke builds the PR image and renders the checked-in [`k8s/vekil.yaml`](../k8s/vekil.yaml), patching only the test namespace, local image/pull policy, and the deterministic provider config used in its second phase. It verifies that the `/healthz` startup probe has a coherent 60–90 second failure budget before liveness/readiness begin. It then deploys without Copilot credentials and verifies that `/healthz` remains live, the liveness probe causes zero restarts, `/readyz` stays gated, the Pod is not Ready, and the Service has no ready endpoint. Finally it rolls out a static configured provider and verifies that the same readiness probe admits the Pod and Service endpoint. The script uses an isolated kubeconfig, bounds cluster/API/port-forward work, and requires the live `kubectl port-forward` PID plus its exact listener log before accepting HTTP responses.

CodeQL code scanning runs from [`.github/workflows/codeql.yaml`](../.github/workflows/codeql.yaml) on pushes to `main`, pull requests to `main`, and a weekly Tuesday 08:37 UTC schedule. It initializes CodeQL for Go, performs a manual `go build ./...`, and uploads the analysis results to GitHub code scanning.

Safe Dependabot updates are handled by [`.github/workflows/dependabot-auto-merge.yaml`](../.github/workflows/dependabot-auto-merge.yaml). It listens for Dependabot pull request lifecycle events, skips drafts, skips pull requests with requested changes, skips major updates, and only auto-approves plus enables auto-merge for semver patch/minor updates from eligible package ecosystems. Grouped Dependabot pull requests are eligible only when `dependabot/fetch-metadata` reports the highest update type as patch or minor.

The auto-merge workflow intentionally skips the `github-actions` and `docker` package ecosystems so workflow/container updates still receive manual review.

For that workflow to work, configure these repository settings and variable:

- enable auto-merge in repository settings
- allow workflow `GITHUB_TOKEN` write permissions for Dependabot pull request runs
- allow GitHub Actions to create and approve pull requests in Actions settings
- set the repository variable `DEPENDABOT_AUTO_MERGE_METHOD` to the explicit merge method to use: `merge`, `rebase`, or `squash`

The configured merge method must also be enabled for the repository; otherwise `gh pr merge --auto` cannot enable auto-merge.

The macOS tray app has its own workflow in [`.github/workflows/macos-app.yaml`](../.github/workflows/macos-app.yaml). It runs `scripts/macos-app-smoke.sh` on a macOS runner, which builds `Vekil.app`, validates the bundle contents, launches the app through Launch Services, verifies it stays up, and then quits it cleanly.

## Release

Tag pushes to [`.github/workflows/release.yaml`](../.github/workflows/release.yaml) use [`.goreleaser.yaml`](../.goreleaser.yaml) to publish the CLI binaries and checksums to GitHub Releases for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`.

RTK version bumps for the optional Docker variant are automated by [`.github/workflows/update-rtk.yaml`](../.github/workflows/update-rtk.yaml). The scheduled workflow runs [`scripts/update-rtk-version.sh`](../scripts/update-rtk-version.sh), updates the pinned `RTK_VERSION` Docker build arg in `Dockerfile.rtk` when `rtk-ai/rtk` publishes a new latest release, validates the rebuilt variant with `make docker-build-rtk` and `make docker-rtk-e2e`, and opens a signed PR.

The same release workflow also:

- builds `vekil-macos-arm64.zip` on a macOS runner and uploads it to the tagged release
- generates and uploads `appcast.xml` for Sparkle update checks
- updates the `vekil` cask in `sozercan/homebrew-repo`
- pushes the multi-arch container image to GHCR
- pushes the `-rtk` multi-arch container image variant to GHCR

To publish the Homebrew cask, configure the repository secret `HOMEBREW_REPO_TOKEN` with push access to `sozercan/homebrew-repo`.

To publish Sparkle updates, configure both `SPARKLE_PUBLIC_ED_KEY` and `SPARKLE_PRIVATE_ED_KEY` in the repository secrets.

## Manual Live Smoke Workflow

The repository also includes a manual `Live Copilot Smoke` workflow in [`.github/workflows/live-copilot-smoke.yaml`](../.github/workflows/live-copilot-smoke.yaml).

It builds the proxy, runs [`scripts/live-compact-smoke.sh`](../scripts/live-compact-smoke.sh) and [`scripts/live-chat-over-responses-smoke.sh`](../scripts/live-chat-over-responses-smoke.sh), installs Codex, Claude Code, and Gemini CLI on a GitHub-hosted runner, and then runs [`scripts/live-cli-smoke.sh`](../scripts/live-cli-smoke.sh).

The compaction smoke script starts the proxy with a non-interactive GitHub token, waits for `/readyz`, selects a currently available OpenAI/Codex model from `/v1/models`, posts to `/v1/responses/compact`, verifies that the response contains a non-empty compaction item, and replays that compaction item through `/v1/responses`.

The Chat-over-Responses smoke selects a model whose native metadata advertises `/responses` but not `/chat/completions` (preferring `gpt-5.6-sol`), so the public Chat request cannot silently use native Chat. It verifies non-streaming and streaming text, terminal usage and `[DONE]`, omitted-`strict` function tools, exact `call_vekil_<22-character-base64url>` IDs, single-call replay, reversed parallel results, and partial continuation that reissues only the missing call. The workflow hard-fails if no Responses-only model is available rather than falling back to a dual-protocol model.

The CLI smoke script starts the proxy with the same token pattern, waits for `/readyz`, selects currently available OpenAI, Anthropic, and Gemini models from `/v1/models`, and runs one file-reading headless check per CLI using isolated temp-home config directories. When a smoke script starts Vekil and `PROXY_PORT` is not set, it allocates an isolated non-default port. Readiness is accepted only after the spawned PID is still live and its log contains the exact `vekil listening` address; every HTTP request and CLI has a deadline, and EXIT/INT/TERM cleanup terminates the whole process group and verifies that the port was released.

This workflow is intentionally provider-specific: it exercises a live Copilot-backed deployment because zero-config startup is the simplest upstream path to run in GitHub Actions. It is useful as a real integration smoke test, but it is not the complete provider matrix for Azure OpenAI, OpenAI Codex, or generic compatible provider configs.

## Live Provider Routing Smoke Workflow

The [`Live Provider Routing Smoke`](../.github/workflows/live-provider-routing-smoke.yaml) workflow runs [`scripts/live-provider-routing-smoke.sh`](../scripts/live-provider-routing-smoke.sh) against two controlled, semantically equivalent, API-key-backed targets that both support native `/responses`. Each target may be `azure-openai` or static `openai-compatible`; Azure URLs must use the OpenAI v1 form ending in `/openai/v1`, while generic targets use bearer auth. Configure these repository variables:

- `LIVE_PROVIDER_ROUTING_PRIMARY_TYPE`, `LIVE_PROVIDER_ROUTING_SECONDARY_TYPE` — `azure-openai` or `openai-compatible`
- `LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL`, `LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL` — API bases before `/responses`
- `LIVE_PROVIDER_ROUTING_PRIMARY_MODEL`, `LIVE_PROVIDER_ROUTING_SECONDARY_MODEL` — physical deployment/model names for the same public contract

Configure these repository secrets explicitly:

- `LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY`
- `LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY`

The harness generates and validates a schema-version-2 config with one fixed public model and an ordered two-target `priority_failover` route. A loopback control proxy first forwards a real request to the primary, then injects an authoritative precommit `429`; the next fresh request must make exactly two upstream sends and complete on the real secondary. It also verifies that `/v1/models` and successful Responses output expose only the public model identity, a primary response ID pins a later request back to that exact target rather than migrating on `429`, unknown state fails locally with no upstream send, and `/stats.json` records the exact target attempts, switch, successful failover, and state-binding hit/miss.

Fork and Dependabot pull requests neutral-skip because GitHub withholds repository secrets. Pull requests also neutral-skip until all eight repository variables/secrets are installed; a manual dispatch with missing configuration fails so it cannot look like a completed live run. Once configured, any controlled-target or routing failure is a hard failure—unlike the rotating Zen free tier, a configured target outage is not treated as neutral. The workflow is separate from deterministic CI. Run the same harness locally after `make build` by exporting the eight variables/secrets above.

For a credential-free generic-provider check, [`scripts/live-zen-smoke.sh`](../scripts/live-zen-smoke.sh) starts the proxy on a non-default port with [`examples/opencode-zen-free.yaml`](../examples/opencode-zen-free.yaml), waits for `/readyz`, and sends one tiny chat completion per OpenCode Zen free model. It needs `curl`, `jq`, and Python for isolated automatic port allocation. Because the Zen free set rotates, the script skips only recognized transient statuses/messages and passes as long as at least one free model responds; unknown statuses and proxy-side faults are hard failures.

## Live OpenCode Zen CLI Smoke Workflow

The [`Live OpenCode Zen Smoke`](../.github/workflows/live-zen-smoke.yaml) workflow runs the **same** `scripts/live-cli-smoke.sh` harness as the Copilot smoke, but in `SMOKE_PROVIDER=zen` mode: it starts vekil with `examples/opencode-zen-free.yaml` (no credentials) and drives real coding-agent CLIs against the OpenCode Zen free tier. Because it needs no secrets, it runs on **every** pull request, **including external-contributor forks** — unlike the Copilot smoke, which self-skips on forks. It is the only live end-to-end coverage of vekil's generic `openai-compatible` provider routing (config loading, bearer auth, static model catalog, and the per-model endpoint allowlist), which zero-config Copilot startup never exercises.

The Zen harness runs the **GitHub Copilot CLI** (offline BYOK mode, `COPILOT_PROVIDER_WIRE_API=completions`), **Claude Code**, and **Gemini CLI**. Copilot is required; Claude and Gemini become required gates whenever they are installed. Each client must independently produce the exact fixture output—one passing client cannot mask another.

For each client/model attempt, a bounded raw chat-completions canary runs first:

- Only upstream conditions evidenced by an HTTP response are skippable: a promotion-ended/rate-limit/temporary-capacity message on an eligible response, HTTP 408/425/429, or HTTP 5xx. Local curl transport failures and timeouts are hard failures because they can indicate a stuck Vekil handler. Unknown statuses, including 404 and 405, are also hard failures.
- After a 200 canary, any CLI nonzero exit, timeout, empty result, or mismatched result is a hard failure unless one bounded second canary on that same model proves that a recognized transient appeared between the first probe and the CLI run.
- A neutral exit 0 is allowed only when no model was reachable **before any client was exercised**. Once a reachable model has exercised a client, every installed client must pass.

OpenAI Codex CLI is intentionally excluded from Zen mode: current Codex is `/responses`-only and always sends a built-in `web_search` tool with no `name`, which the Zen free upstreams reject during the responses→chat translation. The Copilot CLI covers the same `/responses`-style client via its `completions` wire API. Codex remains covered by the Copilot smoke, where it works against the Copilot upstream.

Run it locally after `make build` (requires GitHub Copilot CLI; installed Claude/Gemini CLIs are also enforced):

```bash
SMOKE_PROVIDER=zen PROVIDERS_CONFIG=examples/opencode-zen-free.yaml \
  scripts/live-cli-smoke.sh
```

## Live Copilot Smoke setup

To use the `Live Copilot Smoke` workflow:

1. Create a GitHub token for a user that has GitHub Copilot access.
2. Grant that token the `Copilot Requests` permission.
3. Save it as the repository secret `COPILOT_GITHUB_TOKEN`.
4. Run the `Live Copilot Smoke` workflow from the Actions tab.

This workflow is intentionally separate from the normal CI workflow so pull requests and forked builds remain deterministic and do not depend on live provider credentials.

You can also run the same smoke scripts locally after building `vekil`; the CLI smoke script additionally requires those three CLIs to be installed.

## Extending vekil

### Add a tool optimizer provider

- Add a new `type` case in `newToolOptimizerFromConfig` and a constructor in `proxy/`.
- Keep optimizers opt-in and disabled by default.
- Fail open: errors, timeouts, invalid JSON, or invalid replacements must preserve the original payload.
- Add config validation and tests for the new provider type.
- See [`tool-optimizers.md`](tool-optimizers.md) for the config protocols (`rtk_cli`, `exec_json`, and `noop`).

### Add or extend a provider type

- Register the provider kind in `proxy/providers.go` and its endpoint policy in `proxy/provider_endpoint_policy.go`.
- Keep upstream deployment names internal to provider config; public model IDs remain global.
- Treat `models[].endpoints` and `model_routes[].endpoints` as verified **native** allowlists. Do not advertise untested upstream routes or add `/chat/completions` merely because Vekil can emulate Chat through native Responses.
- Keep Chat backend selection and Responses conversion inside the deep execution seam (`chat_execution.go`, `chat_route*.go`, and `chat_over_responses_*.go`); Anthropic and Gemini handlers should consume canonical Chat results rather than Responses events directly.
- Responses-backed Chat must reject unsupported fields instead of silently dropping them, preserve opaque replay IDs/state bounds, and use the typed internal Chat event transport for streams.
- Preserve startup failure on public-model-ID collisions. For schema version 2 or 3, add new provider/native-endpoint/surface/mode support to the compiled route feature matrix and reject unsupported combinations rather than accepting degraded routes.
- If the provider participates in policy routing, define and validate its `trust_domain`, classifier non-storage capability, forced function-tool support, and live-preflight behavior. V1 policy destinations remain static native-Chat routes; do not silently admit dynamic, Responses-backed, Anthropic, Gemini, multimodal, or multi-tenant policy behavior.
- Cross-link config examples in [`provider-routing.md`](provider-routing.md) instead of duplicating YAML here.
