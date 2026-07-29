#!/usr/bin/env bash

# Re-check the remote annotated tag immediately before a privileged write.
# Usage: verify-remote-tag-state.sh <tag> <expected-tag-object> <expected-commit>

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -eq 3 ]] || release_die "usage: scripts/release/verify-remote-tag-state.sh <tag> <expected-tag-object> <expected-commit>"
tag="$1"
expected_tag_object="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')"
expected_commit="$(printf '%s' "$3" | tr '[:upper:]' '[:lower:]')"
[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || release_die "invalid release tag: ${tag}"
[[ "${expected_tag_object}" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]] || release_die "expected tag object must be a full object ID"
[[ "${expected_commit}" =~ ^[0-9a-f]{40}([0-9a-f]{24})?$ ]] || release_die "expected commit must be a full object ID"
release_require_cmd git

remote="${RELEASE_GIT_REMOTE:-origin}"
remote_state="$(git ls-remote --tags "${remote}" "refs/tags/${tag}" "refs/tags/${tag}^{}")"
remote_tag_object="$(printf '%s\n' "${remote_state}" | awk -v ref="refs/tags/${tag}" '$2 == ref { print tolower($1) }')"
remote_commit="$(printf '%s\n' "${remote_state}" | awk -v ref="refs/tags/${tag}^{}" '$2 == ref { print tolower($1) }')"
[[ -n "${remote_tag_object}" ]] || release_die "remote annotated tag object is missing: ${tag}"
[[ -n "${remote_commit}" ]] || release_die "remote tag is not annotated or cannot be peeled: ${tag}"
[[ "${remote_tag_object}" == "${expected_tag_object}" ]] || release_die "remote tag object changed after preflight: ${tag}"
[[ "${remote_commit}" == "${expected_commit}" ]] || release_die "remote tag commit changed after preflight: ${tag}"

release_write_output tag_object "${remote_tag_object}"
release_write_output commit "${remote_commit}"
printf 'verified remote tag state %s (tag_object=%s commit=%s)\n' "${tag}" "${remote_tag_object}" "${remote_commit}"
