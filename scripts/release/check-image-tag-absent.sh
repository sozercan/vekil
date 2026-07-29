#!/usr/bin/env bash

# Fail closed unless every requested GHCR image tag is absent.
#
# Usage: scripts/release/check-image-tag-absent.sh <ghcr.io/owner/image:tag>...
#
# Optional authentication uses GHCR_USERNAME/GHCR_TOKEN, falling back to
# GITHUB_ACTOR/GITHUB_TOKEN. RELEASE_GHCR_REGISTRY_URL and
# RELEASE_GHCR_TOKEN_URL exist for deterministic local testing; production
# callers should leave them unset. A 404 manifest response means absent. An
# existing tag, authorization failure, rate limit, or ambiguous registry error
# fails the check.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -gt 0 ]] || release_die "usage: scripts/release/check-image-tag-absent.sh <ghcr.io/owner/image:tag>..."
release_require_cmd curl
release_require_cmd python3

registry_url="${RELEASE_GHCR_REGISTRY_URL:-https://ghcr.io}"
token_url="${RELEASE_GHCR_TOKEN_URL:-https://ghcr.io/token}"
username="${GHCR_USERNAME:-${GITHUB_ACTOR:-}}"
token="${GHCR_TOKEN:-${GITHUB_TOKEN:-}}"

umask 077
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-ghcr-check.XXXXXX")"
trap 'release_cleanup_dir "${tmp_dir}"' EXIT
results_file="${tmp_dir}/checked.txt"

for reference in "$@"; do
  if [[ ! "${reference}" =~ ^ghcr\.io/([^:@]+):([A-Za-z0-9_][A-Za-z0-9_.-]{0,127})$ ]]; then
    release_die "image reference must be an explicit ghcr.io path and tag: ${reference}"
  fi
  repository="${BASH_REMATCH[1]}"
  image_tag="${BASH_REMATCH[2]}"
  [[ "${repository}" == "$(printf '%s' "${repository}" | tr '[:upper:]' '[:lower:]')" ]] || release_die "GHCR repository paths must be lowercase: ${reference}"
  [[ "${repository}" != */ && "${repository}" != /* && "${repository}" != *//* && "${repository}" != *[[:space:]]* ]] || release_die "invalid GHCR repository path: ${reference}"

  token_body="${tmp_dir}/token.json"
  token_status_file="${tmp_dir}/token.status"
  curl_args=(
    --silent --show-error --location
    --connect-timeout 15 --max-time 120
    --get
    --data-urlencode "service=ghcr.io"
    --data-urlencode "scope=repository:${repository}:pull"
    --output "${token_body}"
    --write-out '%{http_code}'
  )
  if [[ -n "${token}" ]]; then
    [[ -n "${username}" ]] || release_die "GHCR_USERNAME or GITHUB_ACTOR is required when a registry token is supplied"
    curl_args+=(--user "${username}:${token}")
  fi
  curl "${curl_args[@]}" "${token_url}" >"${token_status_file}"
  token_status="$(cat "${token_status_file}")"
  [[ "${token_status}" == "200" ]] || release_die "GHCR token service returned HTTP ${token_status} for ${repository}"
  bearer="$(python3 - "${token_body}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    payload = json.load(handle)
token = payload.get("token") or payload.get("access_token")
if not isinstance(token, str) or not token:
    raise SystemExit("GHCR token response did not contain a bearer token")
print(token)
PY
)"

  headers="${tmp_dir}/manifest.headers"
  manifest_status="$(curl --silent --show-error \
    --connect-timeout 15 --max-time 120 \
    --header "Authorization: Bearer ${bearer}" \
    --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    --dump-header "${headers}" \
    --output /dev/null \
    --write-out '%{http_code}' \
    "${registry_url%/}/v2/${repository}/manifests/${image_tag}")"

  case "${manifest_status}" in
    404)
      printf '%s\n' "${reference}" >>"${results_file}"
      release_log "confirmed image tag is absent: ${reference}"
      ;;
    200)
      digest="$(awk 'BEGIN { IGNORECASE=1 } /^Docker-Content-Digest:/ { gsub("\\r", "", $2); print $2; exit }' "${headers}")"
      [[ -n "${digest}" ]] || digest="unknown"
      release_die "image tag already exists: ${reference} (${digest})"
      ;;
    *)
      release_die "GHCR manifest lookup for ${reference} returned HTTP ${manifest_status}; absence was not proven"
      ;;
  esac
done

checked_json="$(python3 - "${results_file}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    values = sorted({line.strip() for line in handle if line.strip()})
print(json.dumps(values, separators=(",", ":")))
PY
)"
release_write_output checked_images "${checked_json}"
printf 'confirmed %s GHCR tag(s) are absent\n' "$#"
