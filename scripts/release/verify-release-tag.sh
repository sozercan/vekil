#!/usr/bin/env bash

# Verify a release tag before any privileged release job runs.
#
# Usage: scripts/release/verify-release-tag.sh <tag>
#
# Required environment:
#   RELEASE_SIGNING_PUBLIC_KEY   one OpenSSH public key line
#   RELEASE_SIGNING_PRINCIPAL    allowed SSH signing principal
#   RELEASE_SIGNING_FINGERPRINT  exact SHA256:... ssh-keygen fingerprint
#
# Optional environment:
#   RELEASE_EXPECTED_COMMIT      exact commit expected for the tag; defaults to
#                                GITHUB_SHA when set
#   RELEASE_MAIN_REF             protected mainline ref; defaults to origin/main
#
# When GITHUB_OUTPUT is set, writes: tag, version, commit, tag_object,
# prerelease. The script also requires a SemVer-shaped tag, an annotated tag
# object, a valid SSH Git signature from the approved identity, exact commit
# equality when an expected commit is supplied, and ancestry from mainline.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -eq 1 ]] || release_die "usage: scripts/release/verify-release-tag.sh <tag>"
tag="$1"

release_require_cmd git
release_require_cmd ssh-keygen

[[ -n "${RELEASE_SIGNING_PUBLIC_KEY:-}" ]] || release_die "RELEASE_SIGNING_PUBLIC_KEY is required"
[[ -n "${RELEASE_SIGNING_PRINCIPAL:-}" ]] || release_die "RELEASE_SIGNING_PRINCIPAL is required"
[[ -n "${RELEASE_SIGNING_FINGERPRINT:-}" ]] || release_die "RELEASE_SIGNING_FINGERPRINT is required"
[[ "${RELEASE_SIGNING_PRINCIPAL}" =~ ^[A-Za-z0-9._@+-]+$ ]] || release_die "RELEASE_SIGNING_PRINCIPAL contains unsupported characters"
[[ "${RELEASE_SIGNING_FINGERPRINT}" =~ ^SHA256:[A-Za-z0-9+/=_-]+$ ]] || release_die "RELEASE_SIGNING_FINGERPRINT must be an SHA256 fingerprint"

# SemVer prerelease identifiers are explicit and build metadata is deliberately
# excluded from release tags to keep channel filenames and OCI tags unambiguous.
semver_re='^v([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$'
[[ "${tag}" =~ ${semver_re} ]] || release_die "release tag is not valid vMAJOR.MINOR.PATCH[-PRERELEASE]: ${tag}"
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"
for numeric in "${major}" "${minor}" "${patch}"; do
  [[ "${#numeric}" -eq 1 || "${numeric}" != 0* ]] || release_die "release tag has a numeric identifier with a leading zero: ${tag}"
done
if [[ "${tag}" == *-* ]]; then
  prerelease_value="${tag#*-}"
  IFS='.' read -r -a prerelease_identifiers <<<"${prerelease_value}"
  for identifier in "${prerelease_identifiers[@]}"; do
    if [[ "${identifier}" =~ ^[0-9]+$ && "${#identifier}" -gt 1 && "${identifier}" == 0* ]]; then
      release_die "release tag has a numeric prerelease identifier with a leading zero: ${tag}"
    fi
  done
fi
git check-ref-format "refs/tags/${tag}" >/dev/null || release_die "invalid Git tag ref: ${tag}"

tag_ref="refs/tags/${tag}"
[[ "$(git cat-file -t "${tag_ref}" 2>/dev/null || true)" == "tag" ]] || release_die "release tag must be an annotated tag object: ${tag}"
tag_object="$(git rev-parse --verify "${tag_ref}^{tag}")"
commit="$(git rev-parse --verify "${tag_ref}^{commit}")"

expected_commit="${RELEASE_EXPECTED_COMMIT:-${GITHUB_SHA:-}}"
if [[ -n "${expected_commit}" ]]; then
  expected_commit="$(git rev-parse --verify "${expected_commit}^{commit}")"
  [[ "${commit}" == "${expected_commit}" ]] || release_die "tag commit ${commit} does not match expected commit ${expected_commit}"
fi

main_ref="${RELEASE_MAIN_REF:-origin/main}"
git rev-parse --verify "${main_ref}^{commit}" >/dev/null 2>&1 || release_die "mainline ref is unavailable: ${main_ref}"
git merge-base --is-ancestor "${commit}" "${main_ref}" || release_die "tag commit ${commit} is not an ancestor of ${main_ref}"

key_line_count="$(printf '%s\n' "${RELEASE_SIGNING_PUBLIC_KEY}" | tr -d '\r' | awk 'NF { count++ } END { print count + 0 }')"
[[ "${key_line_count}" == "1" ]] || release_die "RELEASE_SIGNING_PUBLIC_KEY must contain exactly one key line"
public_key="$(printf '%s\n' "${RELEASE_SIGNING_PUBLIC_KEY}" | tr -d '\r' | awk 'NF { print; exit }')"
read -r key_type key_blob _ <<<"${public_key}"
[[ "${key_type}" == ssh-* || "${key_type}" == ecdsa-* || "${key_type}" == sk-* ]] || release_die "unsupported SSH public-key type: ${key_type}"
[[ -n "${key_blob}" ]] || release_die "RELEASE_SIGNING_PUBLIC_KEY is malformed"

umask 077
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-release-tag.XXXXXX")"
trap 'release_cleanup_dir "${tmp_dir}"' EXIT
key_file="${tmp_dir}/release-signing-key.pub"
allowed_signers="${tmp_dir}/allowed-signers"
printf '%s %s\n' "${key_type}" "${key_blob}" >"${key_file}"
actual_fingerprint="$(ssh-keygen -lf "${key_file}" -E sha256 | awk '{print $2}')"
[[ "${actual_fingerprint}" == "${RELEASE_SIGNING_FINGERPRINT}" ]] || release_die "release signing-key fingerprint does not match the reviewed fingerprint"
printf '%s namespaces="git" %s %s\n' "${RELEASE_SIGNING_PRINCIPAL}" "${key_type}" "${key_blob}" >"${allowed_signers}"

git \
  -c gpg.format=ssh \
  -c gpg.ssh.allowedSignersFile="${allowed_signers}" \
  verify-tag "${tag_ref}" >/dev/null || release_die "SSH signature verification failed for ${tag}"

version="${tag#v}"
prerelease=false
[[ "${version}" == *-* ]] && prerelease=true

release_write_output tag "${tag}"
release_write_output version "${version}"
release_write_output commit "${commit}"
release_write_output tag_object "${tag_object}"
release_write_output prerelease "${prerelease}"

printf 'verified release tag %s (commit=%s tag_object=%s prerelease=%s)\n' \
  "${tag}" "${commit}" "${tag_object}" "${prerelease}"
