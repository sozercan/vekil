# Release Security

This document is the operating contract for hardened Vekil releases. It covers GitHub Release assets, GHCR images, the macOS app and Sparkle feed, and the Homebrew cask. The central rule is: **build a distributable once from an approved commit, verify that exact output, publish it without replacement, and retain machine-verifiable evidence.**

The guarantees below apply prospectively from the first release explicitly identified as hardened. Historical release assets are not rebuilt or replaced to retrofit these guarantees.

## Release Invariants

A production release must satisfy all of these conditions:

1. The source ref is an exact SemVer, annotated SSH-signed tag from an approved key. Its commit is on `origin/main` and has the required CI results; privileged jobs re-check the remote tag object and peeled commit immediately before writing or using the Sparkle signing key.
2. Preflight, build, scan, and post-publication verification jobs are read-only. Write permissions and credentials exist only in narrowly scoped jobs protected by GitHub environments.
3. CLI and macOS files are built once, staged, scanned, hashed, and attested. A finalizer only downloads and publishes those staged bytes; it never compiles or packages again.
4. OCI images are built once. Immutable version tags are created and verified by digest before mutable aliases are promoted; promotion never rebuilds an image.
5. Every distributable has a SHA-256 digest, an SBOM, and provenance. Missing or inconsistent evidence blocks publication.
6. A versioned GitHub asset, Git tag, or OCI tag is never replaced. A bad public release is corrected with a new version.
7. GitHub Releases, GHCR, Sparkle, and Homebrew must resolve to the same version and recorded artifact digests.

An SBOM, scan result, or attestation is evidence about an artifact; none of them alone proves that the artifact is safe.

## Repository Configuration

### Required repository variables

Store public trust material as GitHub **repository variables**, not secrets. The release preflight should fail closed when any value is absent or inconsistent.

| Variable | Required value |
| --- | --- |
| `RELEASE_SIGNING_PUBLIC_KEY` | One OpenSSH public key line for the authorized release-tag signer, for example `ssh-ed25519 AAAA...`. Do not store a private key here. |
| `RELEASE_SIGNING_PRINCIPAL` | The exact principal written beside the public key in Git's SSH `allowedSignersFile`, normally a maintainer identity such as an email address. |
| `RELEASE_SIGNING_FINGERPRINT` | The expected `SHA256:...` fingerprint from `ssh-keygen -lf <public-key-file> -E sha256`. Preflight must compare this with the configured public key before trusting it. |
| `SPARKLE_PUBLIC_ED_KEY` | The public EdDSA key embedded in the macOS app for Sparkle update verification. It is public trust metadata, not a secret. |

The SSH key, principal, and fingerprint form one trust record. Update all three in one reviewed rotation, then run release preflight before creating a production tag. A transient mismatch must block releases rather than fall back to an unverified key.

### Protected `release` environment

Create a GitHub environment named `release` and configure:

- required reviewers who can independently confirm the tag, commit, evidence summary, and intended version;
- deployment restrictions that admit only the protected release workflow and release tags;
- the `SPARKLE_PRIVATE_ED_KEY` environment secret;
- any future Apple Developer ID/notarization credentials as environment secrets, never repository-wide secrets; and
- an optional wait timer if maintainers need a final inspection period.

Only the jobs that sign the Sparkle feed or publish GitHub/GHCR state may enter this environment. Read-only builders must not receive its secrets. GitHub release and package writes should use job-level `GITHUB_TOKEN` permissions rather than a broad long-lived token.

### Protected `homebrew` environment

Create a separate GitHub environment named `homebrew` when the tap credential has a distinct owner or approval policy. Configure required reviewers and store `HOMEBREW_REPO_TOKEN` there as a fine-grained token, or replace it with an equivalently scoped GitHub App installation credential. The identity must be limited to `sozercan/homebrew-repo`, may create a release branch and pull request, and must not be able to bypass default-branch protection.

The Vekil repository's release workflow needs no write access to its own contents while preparing the Homebrew change. Never place the tap credential in a build job or expose it to pull-request-controlled code.

### Repository protections

The hardened path also assumes:

- `main` requires the designated CI and security checks;
- release tags cannot be force-updated or deleted casually;
- workflow Actions and release tools are pinned to reviewed immutable versions;
- GitHub immutable releases are enabled once the finalizer has been exercised successfully; and
- per-tag release concurrency never cancels an in-progress publication, while a separate global non-canceling `queue: max` concurrency group serializes mutable-alias promotion so parallel versioned releases cannot race or discard pending `latest` reconciliation.

## Security Gates And Exceptions

Core CI and release preflight run the same pinned release contract, `actionlint`, source secret scan, and `govulncheck ./...`. Because pinned actionlint 1.7.12 predates GitHub's `concurrency.queue` field, CI suppresses only that exact unknown-key diagnostic while the custom contract requires `queue: max`; all other actionlint diagnostics remain blocking. The current Go policy is intentionally stricter than a severity-only gate: any reachable vulnerability reported by `govulncheck` blocks until it is fixed or the reviewed exception mechanism is extended to that scanner. Pull requests also run GitHub dependency review and block newly introduced high or critical vulnerabilities. License changes remain review-only until an explicit allow/deny policy is approved; do not silently invent a license policy in workflow YAML.

Staged CLI/macOS files are scanned with the pinned Gitleaks policy before attestation. Published image digests are scanned with pinned Trivy high/critical vulnerability gates, and their exported runtime filesystems are scanned with Gitleaks. Scanner output containing possible secret material is never published; the secret-scan evidence records only the scanner and pass/fail result.

Reviewed vulnerability exceptions live in `scripts/release/vulnerability-exceptions.json`. Each non-empty entry must name the vulnerability and component, severity, tracking issue, owner, rationale, expiration date, and compensating controls. `scripts/release/validate-vulnerability-exceptions.py` rejects malformed or expired entries in CI and release preflight. An exception identifier must also appear in the applicable scan result and final manifest before it can explain a waived finding; adding an exception record alone does not suppress a scanner.

## Creating A Release Tag

Release tags use SemVer with a leading `v`: stable tags are exactly `vMAJOR.MINOR.PATCH`; prereleases may use an explicit suffix such as `vMAJOR.MINOR.PATCH-rc.1`. Build metadata (`+...`) is not accepted.

Configure Git to use the approved SSH signing key and a local allowed-signers file. The principal and public key must match the reviewed repository variables:

```bash
signing_key=~/.ssh/vekil-release-signing.pub
allowed_signers=~/.config/git/vekil-release-allowed-signers
principal=maintainer@example.com

mkdir -p "$(dirname "$allowed_signers")"
read -r key_type key_blob _ <"$signing_key"
printf '%s namespaces="git" %s %s\n' \
  "$principal" "$key_type" "$key_blob" >"$allowed_signers"
chmod 0600 "$allowed_signers"

git config gpg.format ssh
git config user.signingkey "$signing_key"
git config gpg.ssh.allowedSignersFile "$allowed_signers"
ssh-keygen -lf "$signing_key" -E sha256
```

Create the tag from the reviewed `main` commit. `git tag -s` creates an annotated, signed tag object:

```bash
git fetch origin main --tags
git switch main
git pull --ff-only origin main

tag=v1.2.3
commit=$(git rev-parse HEAD)
git merge-base --is-ancestor "$commit" origin/main

git tag -s -m "Vekil $tag" "$tag" "$commit"
git cat-file -t "$tag"            # must print: tag
git verify-tag "$tag"
git push origin "refs/tags/$tag"
```

Do not use `git tag -f`, retag an existing version, or push before the exact commit's required checks pass. Preflight independently verifies the tag shape, tag-object type, signature, signer principal and fingerprint, selected commit, `origin/main` ancestry, CI state, and absence of conflicting GitHub Release or GHCR version state before any write-capable job starts.

## Build-Once Release Flow

### 1. Read-only preflight

Preflight checks the release identity and proves that the requested version is unused. A malformed, lightweight, unsigned, unapproved, off-mainline, or already-published tag fails before environment approval, write permission, or private signing material is available.

### 2. Build and stage exact outputs

Builders run with `contents: read` and no publication credentials:

- **CLI:** the pinned GoReleaser version emits platform binaries and `checksums.txt` in non-publishing mode.
- **macOS:** a macOS runner verifies the pinned Sparkle input, builds `Vekil.app`, ad-hoc signs it, and creates `vekil-macos-arm64.zip`. Appcast signing is isolated behind the `release` environment.
- **OCI:** BuildKit builds the standard and RTK multi-platform images once. It first publishes only the immutable version tags needed to obtain registry digests; it does not write `latest` or `latest-rtk`.

CLI and macOS outputs are uploaded as short-lived workflow artifacts. The finalizer consumes those exact staged files. It must not run GoReleaser, `go build`, `make build-app`, or another packaging step.

### 3. Generate and verify evidence

Before publication, produce a deterministic `release-manifest.json` containing the repository, source commit, signed tag and tag-object digest, workflow identity, toolchain versions, and—for every output—its filename or OCI reference, media type, size, SHA-256 digest, SBOM reference, attestation subject, and scan status.

Required evidence includes:

- `checksums.txt` and independent size/SHA-256 calculations;
- SPDX or CycloneDX SBOMs for CLI artifacts, the macOS bundle (including Sparkle), and each OCI variant/platform manifest;
- source, artifact, and final-image secret/vulnerability scan results under the release policy;
- artifact smoke-test results; and
- GitHub provenance attestations bound to `sozercan/vekil`, the exact source commit/tag, and the approved release workflow.

Tampering, a missing file, a digest mismatch, a missing SBOM or attestation, an expired security exception, or a failed blocking scan stops the release. Evidence and logs must not contain Sparkle keys, tap credentials, Apple credentials, or unrelated private filesystem data.

### 4. Publish without replacement

The protected finalizer creates the GitHub release once and uploads staged assets without `--clobber`. A new workflow run rejects any existing release. When only failed jobs are rerun after a partial upload, the finalizer may resume an incomplete private draft only after every existing asset name, size, and digest matches the saved local payload; it uploads missing names and never replaces existing bytes. A published versioned asset is never replaced.

OCI publication follows the same no-replacement rule. A pre-existing version tag blocks a new workflow run even if it appears to contain the desired bytes. Partial image-job recovery uses GitHub's rerun-failed-jobs path so already-successful per-variant build jobs are not rebuilt or repushed.

### 5. Promote mutable OCI aliases

Only after the versioned GitHub assets and immutable image digests pass complete verification may the workflow move:

- `ghcr.io/sozercan/vekil:latest` to the approved standard-image digest; and
- `ghcr.io/sozercan/vekil:latest-rtk` to the approved RTK-image digest.

Promotion copies or retags the verified manifest digest; it never rebuilds. A global `queue: max` promotion concurrency group serializes alias changes, and the promoter refuses to run unless its tag is still GitHub's current latest stable release. It requires both mutable aliases to have a known prior digest, records those digests, and restores them if either update or verification fails. If an alias was deleted, restore or seed both aliases under review before rerunning promotion; the workflow will not perform an unrollbackable bootstrap. Prereleases do not promote `latest` aliases.

### 6. Post-publication verification and Homebrew PR

A read-only verifier downloads every GitHub asset, checks it against `release-manifest.json` and `checksums.txt`, verifies provenance, pulls OCI images by recorded digest, inspects OCI SBOM/provenance, validates the Sparkle appcast, and checks the Homebrew PR. Only after that verifier succeeds does a separate stable-only promoter move and re-check the mutable aliases.

Homebrew publication begins only from the finalized `vekil-macos-arm64.zip`:

1. download the public release asset;
2. recompute its SHA-256 instead of trusting an earlier job output;
3. render `Casks/vekil.rb` from the exact version (without the leading `v`) and checksum;
4. run `brew audit --cask --strict` and an install smoke where the runner supports them;
5. push a release-specific branch in `sozercan/homebrew-repo`; and
6. open or update a pull request protected by tap CI and review.

The release workflow must never push directly to the tap's default branch. If unattended publication is later desired, use protected auto-merge after required checks instead of bypassing review.

The renderer has a deliberately small interface:

```bash
scripts/publish-homebrew-cask.sh 1.2.3 \
  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  /path/to/homebrew-repo
```

It accepts only SemVer without `v` or build metadata, requires a 64-character SHA-256, refuses unsafe/symlinked output paths, normalizes the digest to lowercase, and writes deterministic `Casks/vekil.rb` bytes. Re-running it with the same inputs is idempotent.

## Verifying A Download

Install a current authenticated GitHub CLI, download the release assets, and verify both the recorded digest and provenance. Substitute the real tag and asset name:

```bash
tag=v1.2.3
asset=vekil-linux-amd64
commit=REPLACE_WITH_COMMIT_FROM_RELEASE_MANIFEST

gh release download "$tag" --repo sozercan/vekil \
  --pattern "$asset" \
  --pattern checksums.txt \
  --pattern release-manifest.json

grep "  $asset\$" checksums.txt | shasum -a 256 -c -

gh attestation verify "$asset" \
  --repo sozercan/vekil \
  --signer-workflow sozercan/vekil/.github/workflows/release.yaml \
  --source-digest "$commit" \
  --source-ref "refs/tags/$tag"
```

For an OCI image, use the immutable digest recorded in `release-manifest.json`, not a mutable alias:

```bash
digest=sha256:REPLACE_WITH_RELEASE_MANIFEST_DIGEST
commit=REPLACE_WITH_COMMIT_FROM_RELEASE_MANIFEST

gh attestation verify \
  "oci://ghcr.io/sozercan/vekil@$digest" \
  --repo sozercan/vekil \
  --signer-workflow sozercan/vekil/.github/workflows/release.yaml \
  --source-digest "$commit" \
  --source-ref "refs/tags/v1.2.3"
```

`gh attestation verify` validates the GitHub attestation and expected workflow identity. Continue to compare the artifact or image digest with the release manifest; do not treat a valid attestation for an unexpected subject as the intended release.

## Sparkle And macOS Trust

`SPARKLE_PRIVATE_ED_KEY` is used only in the protected `release` environment. Key-consuming steps must disable shell tracing, pass the key through stdin or a short-lived mode-`0600` file, and guarantee cleanup. The key must never appear in logs, caches, workflow artifacts, SBOMs, the app bundle, or the Homebrew repository.

The appcast must reference the exact final ZIP URL, version, byte length, and archive signature, and its trailing Sparkle feed signature must validate. Post-publication verification checks both signatures and the referenced fields against the immutable archive.

Published Vekil app bundles are currently **ad-hoc signed, not Developer ID signed, and not notarized**. Ad-hoc signing supports bundle integrity verification but does not establish an Apple developer identity or satisfy Gatekeeper notarization. The Sparkle signature authenticates updates to an app that already trusts the configured Sparkle public key; it is not a substitute for Apple code signing or notarization.

Developer ID signing and notarization, when adopted, require separately protected Apple credentials, nested-code signing, notarization, stapling, and clean-runner verification. Do not describe the app as notarized until those checks are enforced in the release path.

## Dry Run

Use the manually dispatched release dry-run before changing release tooling and before the first hardened production tag. A dry-run accepts a commit or prerelease tag and performs preflight, all builds, scans, SBOM generation, attestable manifest generation, and smoke tests, but:

- receives no GitHub Release, package-publish, Sparkle production-key, or Homebrew credential;
- uploads only short-lived workflow artifacts and an evidence summary;
- exports OCI layouts instead of changing production tags and loads those exact multi-platform results into an isolated containerd-backed Docker store for amd64/arm64 runtime smokes;
- uses an ephemeral non-production Sparkle key when appcast structure must be exercised; and
- renders the Homebrew cask in a temporary checkout without pushing a branch or opening a PR.

A dry-run success does not authorize a release. A production tag still requires the protected environment approvals and exact production trust checks.

## Incident Response And Rollback

Preserve evidence first. Do not delete or rewrite a public versioned tag, release asset, app ZIP/appcast, or OCI tag to make an incident appear fixed.

### Suspected bad or compromised release

1. Pause new releases and disable or withhold approvals for the `release` and `homebrew` environments.
2. Record the affected tag, commit, workflow run, manifest, asset/image digests, Homebrew PR, Sparkle feed state, and mutable OCI aliases.
3. Add a prominent withdrawal/security notice through release metadata or a security advisory while retaining the original artifacts and attestations for investigation.
4. Close or block an unmerged Homebrew PR. If it merged, use a new reviewed tap PR to remove the cask or point it to a previously verified public version as an explicit rollback.
5. Move only `latest`/`latest-rtk` back to a previously verified immutable digest when container users need immediate mitigation. Verify the aliases after the move.
6. Publish the correction as a new patch version from a reviewed commit and a new signed tag. Let the new release advance GitHub `latest`, Sparkle, Homebrew, and OCI aliases through the normal gates.

A partially assembled **unpublished draft** may be discarded only when no asset or versioned image was made public; retain its workflow logs and evidence. Once any versioned output is public, recovery is by withdrawal plus a new version, not replacement.

### Credential and key rotation

- **Release-tag SSH key:** remove the compromised key from signing systems, update `RELEASE_SIGNING_PUBLIC_KEY`, `RELEASE_SIGNING_PRINCIPAL`, and `RELEASE_SIGNING_FINGERPRINT` under review, and run preflight with the replacement key. Never recreate an old tag with a new signature.
- **Sparkle key:** remove the private key from the `release` environment and pause feed publication. Follow Sparkle's supported key-transition/recovery procedure so already-installed apps can establish the replacement key. If a safe transition cannot be authenticated, publish recovery instructions and a new manually installed app; do not silently replace an old ZIP or appcast.
- **Homebrew credential:** revoke the token or GitHub App installation credential, audit branches and PRs created by that identity, then place a least-privilege replacement only in the `homebrew` environment.
- **Apple credentials:** if Developer ID/notarization is later enabled, revoke affected certificates/API credentials with Apple, replace the environment secrets, and publish a new version after notarization checks pass.

Document who approved the rotation, when the old credential was revoked, the new public fingerprint/identity, and the first release produced with the replacement. Exercise withdrawal, mutable-alias rollback, and emergency patch procedures in a prerelease or non-production repository before relying on them during an incident.
