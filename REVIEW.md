# Review: --quiet CLI flag

## Findings

- Correctness: `effectiveLogLevel` returns `logger.LevelError` whenever quiet mode is enabled, so `--quiet` suppresses debug/info logs while preserving error/fatal logs under the existing logger threshold semantics. This also makes `--quiet` override `--log-level=debug`. Fatal startup errors still print because `LevelFatal` is above the error threshold and `Logger.Fatal` logs before exiting.
- Conventions: The flag follows the existing `registerServeFlags` pattern: `serveFlags` has a `*bool` field, registration uses `fs.Bool`, the default comes from `getEnvBool("QUIET", false)`, and formatting/alignment is consistent with neighboring bool flags such as `responses-ws-enabled`.
- Tests: `TestEffectiveLogLevel`, `TestServeFlagsQuiet`, and `TestServeFlagsQuietEnvDefault` genuinely cover the flag, env default, CLI override of env, and quiet/log-level precedence. I do not see a meaningful missing edge case for this change.
- Scope: `git diff --name-only main..HEAD` shows only `main.go`, `main_test.go`, and `docs/configuration.md` changed. No out-of-scope tracked files were modified.
- Docs: The `docs/configuration.md` row for `--quiet` / `QUIET` is accurate and consistent with the surrounding generic flag table.

## Verification

- PASS: `gofmt -l main.go main_test.go` produced no output.
- PASS: `make build`
- PASS: `make vet`
- PASS: `make test`

VERDICT: LGTM
