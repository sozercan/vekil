#!/usr/bin/env bash

# Query a GitHub release by tag without conflating 404 with API failures.
# Usage: query-github-release.sh <tag> [json-output]
# Exit 0: release exists and validated JSON was written.
# Exit 4: explicit GitHub API 404 (release absent).
# Exit 1: authorization, rate-limit, transport, server, or malformed response.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "${SCRIPT_DIR}/lib.sh"

[[ "$#" -ge 1 && "$#" -le 2 ]] || release_die "usage: scripts/release/query-github-release.sh <tag> [json-output]"
tag="$1"
output="${2:-}"
[[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || release_die "invalid release tag: ${tag}"
repo="${RELEASE_REPOSITORY:-${GITHUB_REPOSITORY:-}}"
[[ "${repo}" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || release_die "RELEASE_REPOSITORY or GITHUB_REPOSITORY must be owner/name"
release_require_cmd curl
release_require_cmd python3
release_require_cmd install

api_url="${RELEASE_GITHUB_API_URL:-${GITHUB_API_URL:-https://api.github.com}}"
encoded_tag="$(python3 - "${tag}" <<'PY'
import sys
import urllib.parse
print(urllib.parse.quote(sys.argv[1], safe=""))
PY
)"
umask 077
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-release-query.XXXXXX")"
trap 'release_cleanup_dir "${tmp_dir}"' EXIT
body="${tmp_dir}/release.json"
headers=(
  --header 'Accept: application/vnd.github+json'
  --header 'X-GitHub-Api-Version: 2022-11-28'
  --header 'User-Agent: vekil-release-query'
)
token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -n "${token}" ]]; then
  headers+=(--header "Authorization: Bearer ${token}")
fi
if ! status="$(curl --silent --show-error \
  --connect-timeout 15 --max-time 120 \
  "${headers[@]}" \
  --output "${body}" --write-out '%{http_code}' \
  "${api_url%/}/repos/${repo}/releases/tags/${encoded_tag}")"; then
  release_die "GitHub release lookup transport failed for ${repo}@${tag}"
fi

case "${status}" in
  200)
    python3 - "${body}" "${tag}" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    payload = json.load(handle)
if payload.get("tag_name") != sys.argv[2]:
    raise SystemExit("GitHub release response tag_name did not match the requested tag")
PY
    if [[ -n "${output}" ]]; then
      install -m 0600 "${body}" "${output}"
    else
      cat "${body}"
    fi
    ;;
  404)
    exit 4
    ;;
  *)
    release_die "GitHub release lookup returned HTTP ${status} for ${repo}@${tag}; absence was not proven"
    ;;
esac
