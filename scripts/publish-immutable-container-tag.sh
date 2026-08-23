#!/usr/bin/env bash

set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

if [[ $# -ne 3 ]]; then
  die "usage: $0 <image> <tag> <sha256-digest>"
fi

image="$1"
tag="$2"
digest="$3"

[[ "${image}" =~ ^[^[:space:]@]+$ ]] || die "invalid container image: ${image}"
[[ "${tag}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid container tag: ${tag}"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "container digest must be sha256 followed by 64 lowercase hexadecimal characters"

reference="${image}:${tag}"
source_reference="${image}@${digest}"
inspect_error="$(mktemp "${TMPDIR:-/tmp}/vekil-container-inspect.XXXXXX")"
trap 'rm -f "${inspect_error}"' EXIT

manifest_digest() {
  python3 -c '
import json
import re
import sys

value = json.load(sys.stdin)
digest = value.get("digest") if isinstance(value, dict) else None
if not isinstance(digest, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
    raise SystemExit("registry inspection did not return a valid manifest digest")
print(digest)
'
}

if existing_manifest="$(docker buildx imagetools inspect "${reference}" --format '{{json .Manifest}}' 2>"${inspect_error}")"; then
  existing_digest="$(printf '%s' "${existing_manifest}" | manifest_digest)"
  [[ "${existing_digest}" == "${digest}" ]] || \
    die "refusing to replace immutable container tag ${reference}: existing ${existing_digest}, candidate ${digest}"
  printf 'Container tag already matches candidate digest: %s@%s\n' "${reference}" "${digest}"
  exit 0
fi

if ! grep -Eiq 'manifest unknown|name unknown|not found|status code 404' "${inspect_error}"; then
  sed 's/^/docker: /' "${inspect_error}" >&2
  die "could not determine whether immutable container tag ${reference} already exists"
fi

docker buildx imagetools create --tag "${reference}" "${source_reference}"
if ! published_manifest="$(docker buildx imagetools inspect "${reference}" --format '{{json .Manifest}}' 2>"${inspect_error}")"; then
  sed 's/^/docker: /' "${inspect_error}" >&2
  die "could not verify published container tag ${reference}"
fi
published_digest="$(printf '%s' "${published_manifest}" | manifest_digest)"
[[ "${published_digest}" == "${digest}" ]] || \
  die "published container tag ${reference} resolved to ${published_digest}, expected ${digest}"
printf 'Published immutable container tag: %s@%s\n' "${reference}" "${digest}"
