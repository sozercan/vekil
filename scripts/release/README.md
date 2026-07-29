# Release security helpers

These helpers are intentionally workflow-oriented, fail closed on ambiguous
remote state, and use only reviewed files under `scripts/release/` plus standard
runner tools. `tool-versions.env` is the lock file for release tooling.

## Tool installation

```bash
scripts/release/install-tools.sh actionlint govulncheck gitleaks syft
```

The installer verifies release-archive SHA-256 values for actionlint, gitleaks,
and Syft. `govulncheck` is built from its exact Go module version only after the
reviewed module and `go.mod` sums match. Binaries go to, in order of precedence:

1. `RELEASE_TOOLS_BIN_DIR`
2. `GOBIN`
3. a runner-local directory when `GITHUB_PATH` exists
4. `$HOME/.local/bin`

The directory is appended to `GITHUB_PATH`, written as `bin_dir` to
`GITHUB_OUTPUT`, and printed on stdout. A local caller must add the printed path
to its parent shell's `PATH`.

## Release identity and remote-state gates

```bash
scripts/release/verify-release-tag.sh v1.2.3
scripts/release/verify-remote-tag-state.sh v1.2.3 <tag-object> <commit>
scripts/release/verify-required-workflows.sh <full-commit> CI CodeQL
scripts/release/query-github-release.sh v1.2.3 release.json
scripts/release/check-image-tag-absent.sh ghcr.io/owner/image:v1.2.3
```

`verify-release-tag.sh` requires:

- `RELEASE_SIGNING_PUBLIC_KEY`: one OpenSSH public-key line;
- `RELEASE_SIGNING_PRINCIPAL`: the exact SSH signing principal;
- `RELEASE_SIGNING_FINGERPRINT`: the reviewed `SHA256:...` fingerprint.

It also checks SemVer shape, annotated-tag type, SSH Git signature, optional
`RELEASE_EXPECTED_COMMIT` (otherwise `GITHUB_SHA`), and ancestry from
`RELEASE_MAIN_REF` (default `origin/main`). It writes `tag`, `version`,
`commit`, `tag_object`, and `prerelease` to `GITHUB_OUTPUT`.

`verify-remote-tag-state.sh` uses `git ls-remote` to compare the current remote annotated-tag object and peeled commit with the preflight outputs. Privileged jobs call it immediately before signing, image publication/attestation, and GitHub Release publication so a moved tag fails closed instead of exploiting a preflight-to-publication race.

`verify-required-workflows.sh` uses `RELEASE_REPOSITORY` or
`GITHUB_REPOSITORY`. Workflow arguments may be display names, IDs, paths, or
filenames. If omitted, `RELEASE_REQUIRED_WORKFLOWS` is split on commas/newlines.
The newest completed run for each workflow and the exact 40-hex commit must be
successful. It writes `commit` and compact `verified_workflows` JSON.

`query-github-release.sh` returns success only for a validated `200` release response and uses a distinct exit status for an explicit `404`. Authentication, transport, rate-limit, and server failures never prove absence; production preflight and draft-resume logic branch only on those explicit states.

`check-image-tag-absent.sh` accepts only explicit `ghcr.io/...:tag` references.
It uses `GHCR_USERNAME`/`GHCR_TOKEN` or `GITHUB_ACTOR`/`GITHUB_TOKEN`. Only a
Registry-v2 manifest `404` proves absence; authorization, rate-limit, and server
errors fail closed. It writes `checked_images` JSON.

## Deterministic manifest

```bash
scripts/release/generate-release-manifest.py \
  --artifact-dir release --output release/release-manifest.json
scripts/release/verify-release-manifest.py \
  --artifact-dir release --manifest release/release-manifest.json
```

Required metadata comes from:

| Manifest field | Environment |
| --- | --- |
| repository | `RELEASE_REPOSITORY` or `GITHUB_REPOSITORY` |
| tag | `RELEASE_TAG` or `GITHUB_REF_NAME` |
| commit | `RELEASE_COMMIT` or `GITHUB_SHA` |
| annotated tag object | `RELEASE_TAG_OBJECT` |
| run ID | `RELEASE_RUN_ID` or `GITHUB_RUN_ID` |
| workflow identity | `RELEASE_WORKFLOW` or `GITHUB_WORKFLOW_REF` |

Optional canonical JSON metadata:

- `RELEASE_IMAGES_JSON`: array of `{repository, tag, digest}` objects;
- `RELEASE_ATTESTATIONS_JSON`: array of `{subject, sha256}` objects;
- `RELEASE_SCANS_JSON`: array of `{name, status, exceptions}` objects;
- `RELEASE_VULNERABILITY_EXCEPTIONS_JSON`: string array of approved IDs;
- `RELEASE_TOOLCHAIN_JSON`: exact string map overriding locked defaults;
- `RELEASE_ARTIFACT_METADATA_JSON`: path-keyed `kind`, `media_type`, `sbom`,
  and `attestation_subject_sha256` overrides;
- `RELEASE_GO_VERSION`: appended to the default locked toolchain map.

Generation sorts every collection and JSON key, omits wall-clock timestamps,
rejects symlinks, and excludes the output manifest from its recursive artifact
set. Offline verification requires canonical bytes, an exact file set, and
matching names, sizes, and SHA-256 values.

## Publication and Sparkle verification

```bash
scripts/release/verify-published-release.py --manifest release/release-manifest.json
scripts/release/verify-sparkle-update.sh \
  vekil-macos-arm64.zip appcast.xml "$SPARKLE_PUBLIC_ED_KEY" \
  "$EXPECTED_URL" "$EXPECTED_VERSION"
```

The published-release verifier defaults repository/tag from the manifest,
requires a non-draft release, compares the exact API asset set (including the
manifest itself), and downloads and re-hashes every asset. `GH_TOKEN` or
`GITHUB_TOKEN` enables private-repository access. `--allow-extra-asset NAME` is
available only for an explicitly reviewed provider-owned asset.

The Sparkle verifier resolves Sparkle 2's parent-item `sparkle:version` child
(with enclosure-attribute compatibility), compares URL/version/archive length,
converts the raw base64 Ed25519 public key to RFC 8410 SPKI, and uses OpenSSL to
verify both the enclosure signature over the archive and the trailing
`sparkle-signatures` comment over its exact declared appcast prefix.

## Policy and contract tests

```bash
scripts/release/test-release-workflow.sh
scripts/release/validate-vulnerability-exceptions.py
scripts/release/scan-secrets.sh . release/
scripts/tests/release-helpers-test.sh
```

Install the locked `actionlint` before running `test-release-workflow.sh`. GitHub's `concurrency.queue` schema was introduced after actionlint 1.7.12; the runner uses one exact ignore for that single unknown-key diagnostic, while the custom contract independently requires `queue: max` on mutable-alias promotion. All other actionlint diagnostics remain blocking. The contract checks every workflow action pin, top-level and job permissions,
protected write/push jobs, checkout credential persistence, publication order,
required tag/evidence/post-publish stages, no-clobber/no direct tap-main push,
exact GoReleaser version, concurrency, and digest-pinned Docker `FROM` inputs.
A read-only `docker/login-action` with `packages: read` is not considered a
publication job; package writes and `push: true` still require a protected
environment. The contract separately proves that the manual dry-run has only
read permissions, no production environment or secret references, no registry
or release publication steps, local OCI outputs, an ephemeral Sparkle key, and
short-lived evidence upload.

`vulnerability-exceptions.json` is reviewed canonical JSON. Each non-empty
exception must include a tracked HTTPS issue, owner, rationale, component,
severity, vulnerability ID, non-expired date, and one or more compensating
controls. The validator prints the active exception-ID array for manifest use.

`scan-secrets.sh` always enables full gitleaks redaction. `.gitleaks.toml`
extends upstream defaults, adds a deterministic release-pipeline canary, and
contains only rule-specific AND allowlists binding each exact synthetic value to its exact reviewed path.
