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
python3 scripts/tests/live-smoke-usage-test.py  # numeric usage summaries and restart accounting
scripts/tests/live-smoke-reliability-test.sh  # deterministic mock-server/fake-CLI gates
scripts/tests/live-chat-over-responses-smoke-test.sh  # deterministic Chat-over-Responses live-harness gates
scripts/tests/live-policy-routing-smoke-test.sh  # deterministic semantic-policy process/cleanup gates
scripts/tests/live-policy-routing-copilot-smoke-test.sh  # deterministic Copilot bridge/model-selection wrapper gate
scripts/tests/live-policy-routing-smoke-test.sh omitted  # repeat the policy matrix without tier effort
scripts/tests/live-policy-routing-responses-effort-smoke-test.sh  # deterministic Responses low/high routing gate
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

- strict version-1/version-2 decoding, duplicate-key rejection, limits, feature-matrix rejection, two-pass provider/route/policy references, normalized public-ID collisions, and offline `vekil config validate`;
- legacy route catalog/unknown-model/retry compatibility and explicit route ordering/catalog identity;
- pristine request body/header/auth construction for every target, including API-key, Entra, bearer, and custom-header switches;
- exact target-attempt and network-send counts for normal attempts, redirects, prewrite failures, ambiguous delivery, protocol recovery, compaction/replay, and compatibility fallback;
- Responses preamble commitment, partial/malformed streams, forced-stream semantic/tool progress, translated Anthropic/Gemini preambles, and client-write races;
- exact state binding before exposure, known/unknown/conflicting state, expiry/eviction/restart/second-process behavior, and pinned WebSocket first-turn/later-turn behavior with no cross-target migration;
- client-request versus physical-attempt accounting, final target attribution, bounded-cardinality/redaction rules, and failed-attempt usage isolation; and
- client disconnect, total deadline, shutdown, response-body cleanup, goroutine termination, and no-overlap/no-new-attempt races.

For large request and replay paths, keep the `64 MiB` request boundary in the deterministic matrix and verify that operation/send budgets prevent compaction, recovery, or fallback from creating an unbounded tree.

### Durable provider-state suite

Durable mode is the default for schema-v2 explicit routes on supported Linux
filesystems and macOS APFS. Run the storage and process suites on both platforms.
Tests use temporary private stores, synthetic credentials, and controlled
loopback providers, never live inference. General route fixtures explicitly use
memory mode so tests cannot open the user's default database:

```bash
go test ./proxy ./server -run '^Test(Durable|StateBindings|StateBindingStats|LoadProvidersConfigFileStateBindings)' -count=1
go test . -run '^Test(DurableStateProcessCrashReopen|StatePruneCommand)' -count=1
go test -race ./... -count=1
GOMAXPROCS=2 go test ./proxy -run '^$' -bench '^BenchmarkStateBindingExposure$' -benchtime=100x -benchmem -count=10
GOMAXPROCS=2 go test ./proxy -run '^$' -bench '^BenchmarkDurableStateLookupAndOpen$' -benchtime=10x -benchmem -count=10
GOMAXPROCS=2 go test ./proxy -run '^$' -bench '^BenchmarkDurableNestedResponseValidation$' -benchtime=5x -benchmem -count=3
```

Coverage includes providers-file-only startup, configuration precedence, private
default-path creation, real binary/process crash and reopen, kills before/after
commit but before exposure, exact issuer and credential-source changes,
private-file validation, logical capacity/tombstones without full preallocation,
dashboard warnings, and offline pruning. Concurrent clients, second-writer
refusal, constructor/drain locks, JSON/SSE and HTTP/native-websocket exposure
failures, and full-input reconnects run on both supported platforms. macOS also
checks descriptor-based ACL refusal and real APFS `F_FULLFSYNC` directory
barriers. JSON tests cover case-variant state fields, competing spellings, and
nesting limits with the default or legacy Go decoder. Raw HTTP, compact, and
WebSocket requests also verify rejection of noncanonical state fields before
dispatch, including after a store reopen, while canonical continuations and
vendor metadata remain supported.

On Linux/amd64, dedicated child tests use one-way seccomp restrictions to inject
real `pwrite64`, `fdatasync`, and `close` errors; the parent and shared filesystem
are not modified. Process kills and syscall failures exercise recovery
boundaries; they do not simulate a host or drive power loss. Cross-compiling
these tests alone does not execute the platform's crash or fault coverage.

The `darwin-launch` CI job also runs the durable proxy/server and menubar tests,
including a legacy JSON backend depth/alias regression check. Temporary provider
and policy smoke proxies explicitly choose memory mode so they do not open a
user's persistent store.

The exposure benchmark calls the production JSON/SSE writers and synchronous
binding store with fresh/repeated batches of 1/8 records and initial occupancy
0/8,192. It reports p50/p95 pre-exposure latency plus database bytes and bytes per
record; those bytes include bbolt overhead and are not the configured capacity.
Lookup/reopen are measured separately. Compare ten controlled memory/durable
samples with fixed CPU settings and an otherwise quiet host. Record raw results,
filesystem, Go version and exact revision privately; do not copy machine data to
the repository. These tests exclude provider/network latency. The prepared-stream
TTFT benchmark alone does not exercise persistence.
The nested-response benchmark measures the complete JSON/SSE writers at depths
1,000/2,000/4,000 with already-recorded state. Compare allocated bytes per
operation to detect diagnostic-path allocation amplification independently of
first-issuance storage latency.

### Conversation migration suite

Opt-in Azure migration uses synthetic providers and private temporary stores.
Run the same suite on macOS APFS and Linux to execute process-kill/reopen tests:

```bash
go test ./proxy -run '^Test(Conversation|LoadProvidersConfigFileConversationMigration)' -count=1
go test -race ./proxy -run '^TestConversation' -count=1
```

Coverage includes HTTP JSON/SSE, local tool history, full-input and response-ID
recovery, immutable older branches, WebSocket reconnect, uncertain execution,
storage faults/capacity/corruption, concurrent admission, offline deletion and
process kills around history commits. Core CI runs these tests on Linux and
the Darwin durable-storage job runs them on macOS. Complete the production gate
before live Azure validation. A live check must use a separate providers file,
database and client workspace, a confirmed prewrite outage, and record the
answering resource, retained context, local side-effect count and next turn.

### Policy-routing safety suite

Schema-v2 policy routing adds a pre-dispatch planner above native OpenAI Chat. The deterministic merge gate must use in-memory classifier adapters and local `httptest` providers; live credentials and provider availability are supplementary, never substitutes for local tests.

Coverage should include:

- schema-v2 route exposure, internal-route non-resolution/catalog exclusion, public-entry/operational-ID collisions, maximum profile count, field ranges, recursive-policy rejection, and schema-v2 feature-field rejection in v1;
- terminal contract intersection across native `/chat/completions` and Responses-backed Chat, profile tier-reasoning membership validation, pinned internal Copilot destination validation, rejection of other dynamic/unsupported provider kinds, and classifier one-target/one-attempt/one-send enforcement;
- exact global/profile mode ceiling behavior, including `off` making zero preflight/classifier calls and observe never changing dispatch;
- bounded canonical facts, UTF-8 truncation, non-text rejection, tool-name-only forwarding, total request cap, and exclusion of credentials, auth headers, provider state, replay IDs, physical routing metadata, parameter schemas, and tool arguments;
- mandatory content-forwarding, trust-domain, cross-domain, non-storage, and retention acknowledgements;
- strict forced `emit_policy_signals` parsing, duplicate-key/extra-field/enum/integer/trailing-content rejection, abstention, and exhaustive deterministic mapper precedence;
- non-blocking per-profile plus global admission, no queue/backlog, partial-admission release, per-profile fairness, cancellation before terminal dispatch, and shutdown cleanup;
- unavailable versus uncertain fallback separation, no fallback caching, infrastructure-only breaker transitions, timeout/content-output immunity, `Retry-After`, cooldown, and one half-open probe;
- sealed operation-plan immutability, classifier/terminal budget separation, exact selected-route sends, selected-tier reasoning override for omitted and explicit client values, function-tool compatibility, identical failover values, translated Anthropic/Responses policy ingress and direct-route non-injection coverage, no cross-tier fallback, and preservation of forced-stream/aggregation behavior for both tiers;
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

`vekil config validate` must remain offline with respect to provider discovery and inference endpoints. When `--providers-config` is an HTTP(S) URL, fetching that config source is the only permitted network request. `vekil config validate --live` is an explicit operator smoke that uses a fixed non-user fixture to verify classifier auth/reachability, forced strict function output, non-storage request acceptance, and one physical send. Tests for both paths should use controlled local servers so CI remains deterministic.

### Chat-over-Responses suite

For the Chat-over-Responses routing, conversion, replay, streaming, and public-ingress matrix, use:

```bash
go test ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1
go test -race ./proxy/ -run 'ChatRoute|ChatExecution|ChatOverResponses|ResponsesBacked|ResponsesChatReplay|ResponsesChatStream|InsightModel' -count=1
go test ./proxy/ -run '^$' -fuzz 'FuzzTranslateChatRequestToResponses' -fuzztime=20s
```

The focused tests cover native-Chat preference, provider-local cold discovery, strict `MAP`/`LOCAL`/`REJECT` input handling, both minted replay ID shapes, full/reordered/partial parallel tool results, restart/state-loss errors, typed event streaming, Anthropic/Gemini ingress, count-token probes, and dashboard-compatible routing.

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

### Native Responses transport benchmark

`BenchmarkResponsesTransportNativeUpstream` compares the default HTTP bridge
with an established native websocket using the same fixed 400-message history.
It reports uploaded bytes per continuation along with latency and allocations:

```bash
GOMAXPROCS=8 go test ./proxy -run '^$' -bench '^BenchmarkResponsesTransportNativeUpstream$' -benchmem -count=3
```

On an Apple M1 Max, darwin/arm64, Go 1.27.1, three one-second local samples
measured:

| Transport | Uploaded bytes/turn | Time/turn | Allocated bytes/turn |
|-----------|--------------------:|----------:|---------------------:|
| HTTP bridge | 140,921 | 591 to 653 µs | 318 to 320 KB |
| Native websocket | 176 | 173 to 178 µs | about 119 KB |

Both endpoints are local deterministic fixtures. These numbers cover warm
transport and history construction; they do not establish live provider latency,
quota changes, model performance, or reconnect behavior. The native transport
remains opt-in, and its [session limits](responses-websocket.md#experimental-native-upstream-transport)
are part of the compatibility contract.

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

GitHub Actions in [`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml) runs on pushes to `main` and pull requests targeting `main`. Its jobs cover lint, tests, the full race detector, the `windows-launch` and `darwin-launch` launcher lifecycle checks, build, vet, a Kubernetes/kind operational smoke, and e2e validation. Every job has a job-level deadline. The test job runs [`scripts/tests/live-smoke-reliability-test.sh`](../scripts/tests/live-smoke-reliability-test.sh) with local mock servers and fake CLIs so stale-listener, timeout, per-client, and descendant-cleanup failures are deterministic, plus real-binary process harnesses: [`scripts/tests/live-provider-routing-smoke-test.sh`](../scripts/tests/live-provider-routing-smoke-test.sh) exercises schema-v2 two-target failover against controlled loopback Responses servers; [`scripts/tests/live-policy-routing-smoke-test.sh`](../scripts/tests/live-policy-routing-smoke-test.sh) exercises semantic-policy modes with and without tier effort, classifier/terminal accounting, controlled failover, automatic non-default port selection, redaction, and process-group cleanup against local Chat-compatible shims; [`scripts/tests/live-policy-routing-copilot-smoke-test.sh`](../scripts/tests/live-policy-routing-copilot-smoke-test.sh) verifies Copilot bridge catalog selection, secret isolation, non-default ports, and descendant cleanup without contacting Copilot; and [`scripts/tests/live-policy-routing-responses-effort-smoke-test.sh`](../scripts/tests/live-policy-routing-responses-effort-smoke-test.sh) proves conflicting public Responses effort is replaced with prompt-selected GPT-5-mini `low`/`high` effort while Haiku classifier requests remain effort-free.

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

The macOS tray app has its own workflow in [`.github/workflows/macos-app.yaml`](../.github/workflows/macos-app.yaml). It runs `go test -race ./cmd/menubar -count=1`, then `scripts/macos-app-smoke.sh` on a macOS runner. The smoke builds `Vekil.app`, validates the bundle contents, launches the app through Launch Services, verifies it stays up, and then quits it cleanly.

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

## Live validation cost controls

Live validation runs on pull-request updates with explicit inexpensive model allowlists and usage reporting. The Copilot protocol/CLI and semantic-policy workflows run on same-repository pull requests targeting `main`, weekly, and on manual dispatch. The provider-routing workflow runs on pull requests when its repository configuration is available and can also be dispatched manually. Fork and Dependabot pull requests skip credentialed checks before checkout. Core CI and the credential-free Zen workflow remain automated for all pull requests. Direct-bearer validation remains restricted to the weekly schedule on `main`, and paid checks do not run again on the post-merge push.

The Copilot scripts use explicit inexpensive model allowlists. A missing required model or capability fails before inference instead of selecting an arbitrary catalog model. Optional Gemini CLI coverage skips when no approved native-Chat Flash model is listed. The policy workflow uses GPT mini terminal routes and Haiku for classification and failover. A separate GPT-5-mini Responses check verifies `low`/`high` effort overrides. Repository model overrides are deliberate wider-coverage choices and must still satisfy endpoint and effort constraints. See [GitHub's current model pricing](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing) before changing them.

Copilot proxy smoke scripts capture `task_usage` before shutdown and publish numeric usage to the Actions step summary, stderr, and an owner-only `usage.json` artifact in the smoke directory. This includes classifier/preflight, compaction, tool continuation, and retry sends. The policy wrapper counts its credentialed bridge once, rather than also adding the delegated proxies' overlapping counters. The carrier restart smoke adds one snapshot from each proxy lifetime. Missing snapshots and unfinished sends are marked partial; missing usage is not estimated, and zero reported billing units do not establish that requests were free. Cached and reasoning tokens are subsets of the input/output totals. Summaries contain no provider topology, request content, or credentials.

## Live Copilot Smoke Workflow

The repository also includes a `Live Copilot Smoke` workflow in [`.github/workflows/live-copilot-smoke.yaml`](../.github/workflows/live-copilot-smoke.yaml). It runs on pull requests targeting `main`, weekly on Monday at 09:31 UTC, and on manual dispatch from the Actions tab. Fork and Dependabot pull requests skip before checkout because repository secrets are unavailable to them. Other runs require `COPILOT_GITHUB_TOKEN`. The workflow uses `pull_request`, never `pull_request_target`, and cancels superseded runs for the same pull request.

It builds the proxy, runs [`scripts/live-compact-smoke.sh`](../scripts/live-compact-smoke.sh) and [`scripts/live-chat-over-responses-smoke.sh`](../scripts/live-chat-over-responses-smoke.sh), installs Codex and Claude Code, runs [`scripts/live-claude-reasoning-carrier-smoke.sh`](../scripts/live-claude-reasoning-carrier-smoke.sh), installs Gemini CLI, and then runs [`scripts/live-cli-smoke.sh`](../scripts/live-cli-smoke.sh).

The compaction smoke script starts the proxy with a non-interactive GitHub token, waits for `/readyz`, requires the approved `gpt-5-mini` model to advertise native `/responses` in `/v1/models`, posts to `/v1/responses/compact`, verifies that the response contains a non-empty compaction item, and replays that compaction item through `/v1/responses`.

The compaction request is also the workflow's availability probe. An exact HTTP 402 response with `error.code: quota_exceeded` makes the script return the dedicated temporary-unavailability status 75; the workflow records a neutral skip and does not install clients or run the remaining credentialed checks. Authentication failures, transport failures, malformed responses, other HTTP 402 errors, and every unknown status remain hard failures.

The Chat-over-Responses smoke requires `gpt-5.6-luna` to advertise native `/responses`, no `/chat/completions`, and function-tool support, so the public Chat request cannot silently use native Chat. It verifies non-streaming and streaming text, terminal usage and `[DONE]`, omitted-`strict` function tools, replay IDs in either minted shape, single-call replay, reversed parallel results, and partial continuation that reissues only the missing call. The versioned shape is `call_vekil_v1_<15-character-nonce>_<upstream-call-id>_<4-character-checksum>`; the opaque fallback is `call_vekil_<22-character-base64url>`. The workflow fails if Luna lacks those capabilities; it does not fall back to Sol or a dual-protocol model.

The Claude reasoning-carrier smoke requires tool-capable, Responses-only `gpt-5.6-luna`, runs Claude Code in `--bare` mode with an isolated home and a `~/.claude/settings.json` that points Anthropic API traffic at Vekil, removes the Copilot credential from the Claude process, and permits only the exact harness-owned Bash command in deny-by-default permission mode. The credentialed proxy binds only to loopback, and its private staged token directory is removed during cleanup while sanitized diagnostics remain available. The command blocks until the harness has fully restarted the proxy, after which the smoke requires an exact final sentinel. The check verifies that Claude reconstructed Vekil's `vekil1` signature delta, returned a `call_vekil_v1_...` tool ID, preserved the carrier without a separate provider mapping, and echoed the matching tool-result ID after Vekil's process-local replay state was gone. The same restarted process then receives carrier-absent histories containing opaque IDs it never minted: one old tool turn, followed by two old turns with a parallel group and reversed results. Both must complete against live Copilot while logs prove `store_missing` visible-transcript degradation rather than carrier restoration. Raw carrier data remains owner-only and is never printed by workflow diagnostics; only fixed assertions, fixed synthetic responses, and a sanitized event summary are shown.

The CLI smoke script starts the proxy with the same token pattern, waits for `/readyz`, requires `gpt-5-mini` for Codex and `claude-haiku-4.5` for Claude from `/v1/models`, and runs one file-reading headless check per CLI using isolated temp-home config directories. It passes the recognized `claude-haiku-4-5` alias to Claude Code so the client does not infer unsupported reasoning effort, and disables thinking for the fixed file-reading task. Vekil normalizes that alias to the catalog ID. Optional Gemini coverage selects only native-Chat `gemini-3.8-flash`, `gemini-3.7-flash`, or `gemini-3.6-flash`. The script defaults `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1` for the isolated Claude process because Copilot does not accept the Advisor Tool beta header. When a smoke script starts Vekil and `PROXY_PORT` is not set, it allocates an isolated non-default port. Readiness is accepted only after the spawned PID is still live and its log contains the exact `vekil listening` address; every HTTP request and CLI has a deadline, and EXIT/INT/TERM cleanup terminates the whole process group and verifies that the port was released.

This workflow is intentionally provider-specific: it exercises a live Copilot-backed deployment because zero-config startup is the simplest upstream path to run in GitHub Actions. It is useful as a real integration smoke test, but it is not the complete provider matrix for Azure OpenAI, OpenAI Codex, or generic compatible provider configs.

## Live Provider Routing Smoke Workflow

The [`Live Provider Routing Smoke`](../.github/workflows/live-provider-routing-smoke.yaml) workflow runs on pull requests targeting `main` and on manual dispatch. It runs [`scripts/live-provider-routing-smoke.sh`](../scripts/live-provider-routing-smoke.sh) against two controlled, semantically equivalent, API-key-backed targets that both support native `/responses`. Each target may be `azure-openai` or static `openai-compatible`; Azure URLs must use the OpenAI v1 form ending in `/openai/v1`, while generic targets use bearer auth. Configure these repository variables:

- `LIVE_PROVIDER_ROUTING_PRIMARY_TYPE`, `LIVE_PROVIDER_ROUTING_SECONDARY_TYPE` — `azure-openai` or `openai-compatible`
- `LIVE_PROVIDER_ROUTING_PRIMARY_BASE_URL`, `LIVE_PROVIDER_ROUTING_SECONDARY_BASE_URL` — API bases before `/responses`
- `LIVE_PROVIDER_ROUTING_PRIMARY_MODEL`, `LIVE_PROVIDER_ROUTING_SECONDARY_MODEL` — physical deployment/model names for the same public contract

Configure these repository secrets explicitly:

- `LIVE_PROVIDER_ROUTING_PRIMARY_API_KEY`
- `LIVE_PROVIDER_ROUTING_SECONDARY_API_KEY`

The harness generates and validates a schema-version-2 config with one fixed public model and an ordered two-target `priority_failover` route. A loopback control proxy first forwards a real request to the primary, then injects an authoritative precommit `429`; the next fresh request must make exactly two upstream sends and complete on the real secondary. It also verifies that `/v1/models` and successful Responses output expose only the public model identity, a primary response ID pins a later request back to that exact target rather than migrating on `429`, unknown state fails locally with no upstream send, and `/stats.json` records the exact target attempts, switch, successful failover, and state-binding hit/miss. The pinned Azure case waits for reset and exhausts its two-send budget on the primary with zero switches; a generic primary makes one rejected send.

Fork and Dependabot pull requests skip before checkout. Other pull requests skip when any of the eight repository variables/secrets are missing; manual dispatch fails on missing configuration. Once configured, a controlled-target or routing failure is a hard failure. The workflow is separate from deterministic CI. Run the same harness locally after `make build` by exporting the eight variables/secrets above.

## Live Semantic Policy Routing Smoke Workflows

The [`Live Copilot Semantic Policy Routing Smoke`](../.github/workflows/live-policy-routing-copilot-smoke.yaml) workflow has the uniquely named job `semantic-policy-e2e`. It runs on pull requests targeting `main`, weekly on Monday at 09:47 UTC, and on manual dispatch. It reuses the repository's existing `COPILOT_GITHUB_TOKEN`. [`scripts/live-policy-routing-copilot-smoke.sh`](../scripts/live-policy-routing-copilot-smoke.sh) starts a private zero-config Vekil bridge backed by Copilot, reads its `/v1/models` catalog, selects native-Chat models, and delegates to the common [`scripts/live-policy-routing-smoke.sh`](../scripts/live-policy-routing-smoke.sh) acceptance harness.

It then runs [`scripts/live-policy-routing-responses-effort-smoke.sh`](../scripts/live-policy-routing-responses-effort-smoke.sh) against the same bridge with both terminal tiers pinned to `gpt-5-mini` over `/responses`. A simple prompt carries conflicting client `high` but must execute with tier `low`; a complex prompt carries conflicting client `low` but must execute with tier `high`. Haiku classifies both prompts through native Chat. The capture shim records only endpoint/model/effort/store metadata and verifies terminal requests preserve `store: false` while classifier requests omit unsupported effort and store fields.

The bridge is intentional even though production schema-v2 policy profiles can target pinned models on a dynamic `type: copilot` provider directly, including Responses-backed Chat models. It gives the common smoke harness independently controllable static targets and fault injection, keeps the real Copilot token in one private bridge process, and permits metadata-only capture of the exact terminal `/responses` request without exposing credentials or request content. The wrapper removes `COPILOT_GITHUB_TOKEN` from delegated harness environments, gives the bridge a private token directory, auto-selects a non-default loopback port, and verifies bridge/process-group cleanup.

The common harness keeps the observe-mode baseline at a 512-token ceiling. Every padded synthetic routing task ends with a fixed-response instruction outside the bounded classifier projection, so truncation still forces conservative powerful routing without asking terminal models to spend their budgets solving meaningless padding.

For the broad matrix, the wrapper requires native `/chat/completions` support. Default selection is limited to:

- lightweight: `gpt-5-mini`, then `gpt-5.4-mini`;
- classifier: `claude-haiku-4.5`, which emits the required function call within the 256-token classifier budget. GPT-5-mini exhausted that budget in reasoning during live validation;
- powerful primary: `gpt-5-mini`, then `gpt-5.4-mini`; and
- powerful secondary: `claude-haiku-4.5`, distinct from the primary.

Haiku does not support reasoning effort, so the broad matrix omits it from both tiers. The common script accepts optional `LIVE_POLICY_ROUTING_LIGHTWEIGHT_REASONING_EFFORT` and `LIVE_POLICY_ROUTING_POWERFUL_REASONING_EFFORT` values only as a pair. Before either matrix sends requests, the wrapper verifies the broad models and requires `gpt-5-mini` to advertise `/responses` plus both `low` and `high`. The standalone Responses check also validates its terminal and classifier models before inference.

Both Copilot policy checks preserve the classifier's `temperature: 0`; terminal routes still drop sampling parameters. The wrapper sets `LIVE_POLICY_ROUTING_CLASSIFIER_DROP_SAMPLING_PARAMS=false` for this. Standalone common-script callers can set the same option when their classifier supports temperature; it defaults to `true`.

Optional repository variables pin a model instead of using dynamic selection:

- `LIVE_POLICY_ROUTING_COPILOT_LIGHTWEIGHT_MODEL`
- `LIVE_POLICY_ROUTING_COPILOT_CLASSIFIER_MODEL`
- `LIVE_POLICY_ROUTING_COPILOT_POWERFUL_PRIMARY_MODEL`
- `LIVE_POLICY_ROUTING_COPILOT_POWERFUL_SECONDARY_MODEL`

Both policy tests declare `classifier_no_store_supported: false` and set the synthetic profile's `allow_provider_retention: true` acknowledgement. Vekil removes the classifier `store` field before forwarding to Haiku. The capture shim forwards the body unchanged, so its assertions verify the actual request sent to the bridge. Both tests send only fixed synthetic content; they do not establish an external retention guarantee.

The common native-Chat live matrix covers:

- offline config validation followed by the explicit one-send live classifier preflight;
- `off` mode using the configured lightweight baseline without a runtime classifier send;
- `observe` mode serving the baseline while recording a bounded asynchronous shadow decision;
- `enforce` mode routing a bounded one-file task to lightweight and a complex or truncated task to powerful;
- forced function tools, tool-result continuation, and parallel distinct function calls;
- powerful streaming with canonical public model identity and exactly one `[DONE]`;
- retry-safe within-powerful-tier failover through a loopback validation shim that injects an authoritative precommit `429`;
- representative local rejections with zero classifier and terminal sends; and
- `/stats.json`, response, header, log, generation-hash, prompt/tool sentinel, upstream request-ID, and internal-topology redaction checks.

In the Copilot matrix, the powerful targets are distinct models but share one Copilot service and loopback bridge. That proves sealed tier selection, retry accounting, target switching, and public-identity behavior; it does **not** prove independent cross-provider availability.

The focused Responses matrix uses public model `vekil-live-semantic-effort`. Both tiers use the same physical `gpt-5-mini` model with different profile-owned effort. Captured requests prove that prompt classification selects `low` versus `high` even when the client requests the opposite effort.

True cross-provider coverage remains available through the manual [`Live Multi-Provider Semantic Policy Routing Smoke`](../.github/workflows/live-policy-routing-smoke.yaml) workflow and its `semantic-policy-multiprovider-e2e` job. Configure these repository variables for that workflow:

- `LIVE_POLICY_ROUTING_LIGHTWEIGHT_TYPE` — `azure-openai` or `openai-compatible`
- `LIVE_POLICY_ROUTING_LIGHTWEIGHT_BASE_URL` — the lightweight provider API base before `/chat/completions`; Azure uses the OpenAI v1 form ending in `/openai/v1`
- `LIVE_POLICY_ROUTING_LIGHTWEIGHT_MODEL` — the physical lightweight model/deployment name
- `LIVE_POLICY_ROUTING_LIGHTWEIGHT_REASONING_EFFORT` — the policy-owned effort forced after the lightweight tier is selected, for example `low`
- `LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_TYPE` — `azure-openai` or `openai-compatible`
- `LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_BASE_URL` — the primary powerful provider API base before `/chat/completions`
- `LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_MODEL` — the physical primary powerful model/deployment name
- `LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_TYPE` — `azure-openai` or `openai-compatible`
- `LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_BASE_URL` — the secondary powerful provider API base before `/chat/completions`
- `LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_MODEL` — the semantically equivalent secondary powerful model/deployment name
- `LIVE_POLICY_ROUTING_POWERFUL_REASONING_EFFORT` — the policy-owned effort forced after the powerful tier is selected, for example `max`; both powerful targets must support it
- `LIVE_POLICY_ROUTING_CLASSIFIER_MODEL` — the classifier deployment on the powerful-primary provider
- `LIVE_POLICY_ROUTING_CLASSIFIER_NO_STORE_SUPPORTED` — `true` only after confirming support; otherwise set `false` and explicitly set `LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION=true`
- `LIVE_POLICY_ROUTING_ALLOW_PROVIDER_RETENTION` — optional, defaults to `false`

Configure these repository secrets for the manual workflow:

- `LIVE_POLICY_ROUTING_LIGHTWEIGHT_API_KEY`
- `LIVE_POLICY_ROUTING_POWERFUL_PRIMARY_API_KEY`
- `LIVE_POLICY_ROUTING_POWERFUL_SECONDARY_API_KEY`

The generated temporary schema-v2 config references `api_key_env` names and never embeds credential values. The powerful primary and secondary must share the same public Chat contract. A configured target outage, protocol incompatibility, classifier failure, privacy leak, wrong tier, or accounting mismatch is a hard failure.

Both workflows use isolated loopback ports, reject port `1337`, retry bounded address-in-use races, and verify that every proxy, bridge, and shim listener is released. Do not change either workflow to `pull_request_target`. The Copilot workflow uses ordinary `pull_request` events and skips fork and Dependabot pull requests before checkout. Other Copilot runs require `COPILOT_GITHUB_TOKEN`. The manual multi-provider workflow fails when its configuration is missing.

If the private Copilot bridge records HTTP 402 while either delegated Copilot policy harness is failing, the wrapper returns temporary-unavailability status 75 and the workflow neutral-skips that live coverage. Other harness failures, including 401/403 credentials, protocol mismatches, timeouts, and unknown upstream statuses, remain hard failures.

These smokes incur real provider cost and are bounded acceptance coverage, not the 75-task pilot/holdout or 5,000-observation production-enforcement evaluation described in the policy-routing release gate. Failure diagnostics are allowlisted, redacted, and truncated; raw generated provider configs are not uploaded.

Run the Copilot-backed matrix locally after `make build`:

```bash
COPILOT_GITHUB_TOKEN=... scripts/live-policy-routing-copilot-smoke.sh
```

Run the true multi-provider harness locally by exporting its variables and secrets:

```bash
scripts/live-policy-routing-smoke.sh
```

For a credential-free generic-provider check, [`scripts/live-zen-smoke.sh`](../scripts/live-zen-smoke.sh) starts the proxy on a non-default port with [`examples/opencode-zen-free.yaml`](../examples/opencode-zen-free.yaml), waits for `/readyz`, and sends one tiny chat completion per OpenCode Zen free model advertising `/chat/completions`. It needs `curl`, `jq`, and Python for isolated automatic port allocation. Because the Zen free set rotates, the script skips only recognized transient statuses/messages and passes as long as at least one free model responds; unknown statuses and proxy-side faults are hard failures.

## Live OpenCode Zen CLI Smoke Workflow

The [`Live OpenCode Zen Smoke`](../.github/workflows/live-zen-smoke.yaml) workflow runs the **same** `scripts/live-cli-smoke.sh` harness as the Copilot smoke, but in `SMOKE_PROVIDER=zen` mode: it starts vekil with `examples/opencode-zen-free.yaml` (no credentials) and drives real coding-agent CLIs against the OpenCode Zen free tier. Because it needs no secrets, it runs on **every** pull request, **including external-contributor forks**. Paid Copilot checks run separately on same-repository pull requests, weekly schedules, and manual dispatch. It is the only live end-to-end coverage of vekil's generic `openai-compatible` provider routing (config loading, bearer auth, static model catalog, and the per-model endpoint allowlist), which zero-config Copilot startup never exercises.

Before starting Vekil, the workflow fetches OpenCode's published Zen documentation and uses [`scripts/parse-opencode-zen-free-models.sh`](../scripts/parse-opencode-zen-free-models.sh) to join the endpoint and pricing tables by display label. Only rows whose input and output prices are both labeled `Free` are eligible for the smoke. The harness intersects that parsed set with the checked-in static example, so aliases such as Ox Alpha's `x-preview-f-free` do not depend on an ID suffix and models that lose their free label are not exercised anonymously.

CI copies the single-provider example into a temporary config and adds
`User-Agent: vekil-live-smoke/1` plus one UUID in `x-opencode-session` for the
smoke run. The stable session header lets the upstream retain routing and cache
affinity. The generated config is validated offline before the proxy starts.

The separate [`Update OpenCode Zen Free Models`](../.github/workflows/update-opencode-zen-free.yaml) workflow runs daily on trusted `main` code. It resolves OpenCode's mutable `dev` branch to an exact commit, downloads only that revision's `zen.mdx`, and runs [`scripts/update-opencode-zen-free-config.sh`](../scripts/update-opencode-zen-free-config.sh) to replace the marked model block in the example. The renderer sorts IDs, preserves the rest of the file, rejects duplicates, bounds the catalog, and accepts only `/chat/completions` and `/responses`; free `/messages` and `/systemone` models cannot be emitted under `openai-compatible`. Changed output is validated offline and proposed in a signed PR rather than written directly to `main`.

To reproduce an update locally:

```bash
curl --fail --location \
  https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx \
  | scripts/update-opencode-zen-free-config.sh -
make build
./vekil config validate --providers-config examples/opencode-zen-free.yaml
```

The Zen harness runs the **GitHub Copilot CLI** (offline BYOK mode, `COPILOT_PROVIDER_WIRE_API=completions`), **Claude Code**, and **Gemini CLI**. Copilot is required; Claude and Gemini become required gates whenever they are installed. Each client must independently produce its exact client-specific prompt sentinel on at least one reachable candidate—one passing client cannot mask another, and one weak free model cannot fail a client that works through another candidate. Zen uses a direct text-only prompt and selects only configured free models whose parsed and proxy metadata both advertise `/chat/completions`; Responses-only entries remain routable but are not candidates for this shared Chat-oriented harness. The selected free Chat models are not assumed to support reliable coding-tool execution; the credentialed Copilot smoke retains the file-reading fixture that exercises tools.

After Claude Code 2.1.212 regressed headless output, the workflow pins a verified Claude Code version. Move the pin only after the candidate version passes the live smoke.

For each client/model attempt, a bounded raw chat-completions canary runs first:

- Only upstream conditions evidenced by an HTTP response are skippable: an exact listed-model-unavailable HTTP 400, a promotion-ended, exact model-no-longer-supported, rate-limit, or temporary-capacity message on an eligible response, HTTP 408/425/429, or HTTP 5xx. Local curl transport failures and timeouts are hard failures because they can indicate a stuck Vekil handler. Other HTTP 400 responses and unknown statuses, including 404 and 405, are hard failures.
- After a 200 canary, any CLI nonzero exit, timeout, empty result, or mismatched result gets one bounded second canary on that same model. A recognized transient skips the candidate; a still-reachable but incompatible candidate is recorded and the client must pass another candidate. The job fails if a client exhausts the reachable set without an exact pass.
- A neutral exit 0 is allowed only when no model was reachable **before any client was exercised**. Once a reachable model has exercised a client, every installed client must pass.

OpenAI Codex CLI is intentionally excluded from this Zen harness because its native wire API requires `/responses`, while the shared Copilot/Claude/Gemini matrix deliberately tests `/chat/completions`. Codex can use a configured Zen free model that advertises native `/responses` (currently `muse-spark-1.2-contributor-free`), and Vekil forwards that request directly rather than using Responses-to-Chat translation. Codex remains covered by the Copilot smoke, where it works against the Copilot upstream.

Run it locally after `make build` (requires GitHub Copilot CLI; installed Claude/Gemini CLIs are also enforced):

```bash
SMOKE_PROVIDER=zen PROVIDERS_CONFIG=examples/opencode-zen-free.yaml \
  scripts/live-cli-smoke.sh
```

## Live Copilot Direct-Bearer Smoke Workflow

The [`Live Copilot Direct Bearer Smoke`](../.github/workflows/live-copilot-direct-bearer-smoke.yaml) workflow is focused credentialed coverage for direct `COPILOT_GITHUB_TOKEN` authentication. It uses a dedicated fine-grained PAT to verify that Vekil returns the original environment token from `GetToken` without contacting `api.github.com`, a second call uses the in-memory cache, and `GetResponsesToken` preserves the same direct bearer. The test then reads Copilot's live `/models` catalog, selects an advertised `/responses` model from the explicit `gpt-5.6-luna`, `gpt-5-mini`, `gpt-5.4-mini` allowlist, and requires one bounded `store: false` inference to return a completed response with non-empty output text. It records exactly those two successful upstream requests and verifies that neither `access-token` nor `api-key.json` is written.

The credentialed workflow runs only on its weekly Monday 09:17 UTC schedule from `main`. It has no push, pull-request, or ref-selectable manual trigger. Package initialization and `TestMain` therefore cannot expose the dedicated PAT to task-branch code through this workflow. Pull requests exercise credential classification, zero-request resolution, legacy fallback, and persistence rules through deterministic tests in `auth/authenticator_test.go`.

Configure the repository secret `COPILOT_FINE_GRAINED_PAT` with a fine-grained personal access token for an account with Copilot access and the **Copilot Requests** permission. A missing secret is a hard workflow failure.

Run the exact check locally without printing the credential:

```bash
LIVE_COPILOT_DIRECT_BEARER_TEST=1 \
  COPILOT_GITHUB_TOKEN=... \
  go test ./auth -run '^TestLiveEnvAccessTokenDirectBearer$' -count=1 -v
```

## Live Copilot workflows setup

The `Live Copilot Smoke` and `Live Copilot Semantic Policy Routing Smoke` workflows share one supported direct GitHub credential:

1. Create a GitHub token for a user that has GitHub Copilot access.
2. Grant that token the `Copilot Requests` permission.
3. Save it as the repository secret `COPILOT_GITHUB_TOKEN`.
4. Both workflows run automatically on same-repository pull requests targeting `main` and weekly from `main`. Run either workflow from the Actions tab when needed. Fork and Dependabot pull requests skip credentialed checks.

The direct-bearer workflow deliberately uses the separate `COPILOT_FINE_GRAINED_PAT` credential described above so fine-grained-PAT authentication remains independently covered.

The credentialed Copilot workflows remain separate from deterministic core CI. Eligible runs fail when their required credential is unavailable. The direct-bearer workflow is restricted to `main` as described above.

The credentialed workflows also neutral-skip only explicitly detected Copilot billing unavailability: the general smoke requires the exact `quota_exceeded` response described above, while the semantic-policy wrapper requires a bridge-recorded HTTP 402 concurrent with a harness failure. This prevents an exhausted repository account from presenting as a code regression without weakening credential, transport, or protocol failures.

You can also run the same smoke scripts locally after building `vekil`; the CLI smoke script additionally requires Codex and Claude Code, plus Gemini CLI when an approved Gemini model is listed.

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
- Responses-backed Chat must reject unsupported fields instead of silently dropping them, preserve replay IDs unchanged along with their state bounds, and use the typed internal Chat event transport for streams.
- Preserve startup failure on public-model-ID collisions. For schema version 2, add new provider/native-endpoint/surface/mode support to the compiled route feature matrix and reject unsupported combinations rather than accepting degraded routes.
- If the provider participates in policy routing, define and validate its `trust_domain`, classifier non-storage capability, forced function-tool support, and live-preflight behavior. Policy destinations may use native Chat or the bounded Chat-over-Responses adapter. Copilot is the only catalog-driven explicit-route exception; pinned targets must be validated against discovery and suppressed from the provider's legacy catalog. Do not silently admit other dynamic providers, Anthropic, Gemini, multimodal, or multi-tenant policy behavior.
- Cross-link config examples in [`provider-routing.md`](provider-routing.md) instead of duplicating YAML here.
