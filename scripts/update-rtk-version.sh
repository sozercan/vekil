#!/usr/bin/env bash

set -euo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

write_output() {
  local key="$1"
  local value="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "${key}" "${value}" >>"${GITHUB_OUTPUT}"
  fi
}

require_cmd python3

DOCKERFILE="${RTK_UPDATE_DOCKERFILE:-Dockerfile.rtk}"
REPO="${RTK_UPDATE_REPO:-rtk-ai/rtk}"

[[ -f "${DOCKERFILE}" ]] || die "Dockerfile not found: ${DOCKERFILE}"

current="$(sed -nE 's/^ARG RTK_VERSION=([^[:space:]]+).*/\1/p' "${DOCKERFILE}" | head -n 1)"
[[ -n "${current}" ]] || die "could not find ARG RTK_VERSION in ${DOCKERFILE}"

if [[ -n "${RTK_VERSION_OVERRIDE:-}" ]]; then
  latest_tag="${RTK_VERSION_OVERRIDE}"
else
  latest_tag="$(python3 - "${REPO}" <<'PY'
import json
import os
import sys
import urllib.request

repo = sys.argv[1]
request = urllib.request.Request(
    f"https://api.github.com/repos/{repo}/releases/latest",
    headers={"Accept": "application/vnd.github+json", "User-Agent": "vekil-rtk-version-check"},
)
token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
if token:
    request.add_header("Authorization", f"Bearer {token}")

with urllib.request.urlopen(request, timeout=30) as response:
    release = json.load(response)

tag = release.get("tag_name", "").strip()
if not tag:
    raise SystemExit("latest release did not include tag_name")
print(tag)
PY
)"
fi

latest="${latest_tag#v}"
[[ -n "${latest}" ]] || die "latest RTK version is empty"

write_output current-version "${current}"
write_output latest-version "${latest}"

if [[ "${current}" == "${latest}" ]]; then
  log "RTK is already up to date (${current})"
  write_output changed false
  exit 0
fi

log "Updating RTK from ${current} to ${latest}"
python3 - "${DOCKERFILE}" "${latest}" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
latest = sys.argv[2]
text = path.read_text()
updated, count = re.subn(r"^ARG RTK_VERSION=\S+", f"ARG RTK_VERSION={latest}", text, count=1, flags=re.MULTILINE)
if count != 1:
    raise SystemExit(f"expected to update exactly one RTK_VERSION line, updated {count}")
path.write_text(updated)
PY

write_output changed true
