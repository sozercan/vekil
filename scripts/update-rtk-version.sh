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

metadata="$(python3 - "${DOCKERFILE}" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
patterns = {
    "RTK_VERSION": r"^ARG RTK_VERSION=([^\s]+)[ \t]*$",
    "amd64 SHA-256": r'^[ \t]*rtk_sha256_amd64="([^"]+)";.*$',
    "arm64 SHA-256": r'^[ \t]*rtk_sha256_arm64="([^"]+)";.*$',
}
values = []
for name, pattern in patterns.items():
    matches = re.findall(pattern, text, flags=re.MULTILINE)
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one {name} value in {path}, found {len(matches)}")
    values.append(matches[0])

for name, digest in zip(("amd64", "arm64"), values[1:]):
    if not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise SystemExit(f"invalid pinned RTK {name} SHA-256 in {path}")

print(values[0])
PY
)"
current="${metadata}"

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
[[ "${current}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "current RTK version is not stable SemVer: ${current}"
[[ "${latest}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "latest RTK version is not stable SemVer: ${latest}"

write_output current-version "${current}"
write_output latest-version "${latest}"

if [[ "${current}" == "${latest}" ]]; then
  log "RTK is already up to date (${current})"
  write_output changed false
  exit 0
fi

if [[ -n "${RTK_UPDATE_ASSET_BASE_URL:-}" ]]; then
  asset_base_url="${RTK_UPDATE_ASSET_BASE_URL%/}"
else
  asset_base_url="https://github.com/${REPO}/releases/download/v${latest}"
fi

log "Fetching and verifying RTK ${latest} release assets"
hashes="$(python3 - "${asset_base_url}" <<'PY'
import hashlib
import re
import sys
import urllib.parse
import urllib.request

base_url = sys.argv[1].rstrip("/")
archives = {
    "amd64": "rtk-x86_64-unknown-linux-musl.tar.gz",
    "arm64": "rtk-aarch64-unknown-linux-gnu.tar.gz",
}


def asset_url(name: str) -> str:
    return f"{base_url}/{urllib.parse.quote(name, safe='')}"


def request(name: str):
    return urllib.request.Request(
        asset_url(name),
        headers={"Accept": "application/octet-stream", "User-Agent": "vekil-rtk-version-update"},
    )


with urllib.request.urlopen(request("checksums.txt"), timeout=60) as response:
    manifest_bytes = response.read(1024 * 1024 + 1)
if len(manifest_bytes) > 1024 * 1024:
    raise SystemExit("RTK checksums.txt exceeds the 1 MiB safety limit")

try:
    manifest = manifest_bytes.decode("utf-8")
except UnicodeDecodeError as error:
    raise SystemExit("RTK checksums.txt is not valid UTF-8") from error

expected = {}
line_pattern = re.compile(r"^([0-9A-Fa-f]{64})[ \t]+[*]?(.+?)\s*$")
for line in manifest.splitlines():
    match = line_pattern.fullmatch(line)
    if not match:
        continue
    digest, filename = match.groups()
    if filename in archives.values():
        if filename in expected:
            raise SystemExit(f"duplicate checksum entry for {filename}")
        expected[filename] = digest.lower()

missing = sorted(set(archives.values()) - set(expected))
if missing:
    raise SystemExit(f"checksums.txt is missing required RTK assets: {', '.join(missing)}")

computed = {}
for architecture, filename in archives.items():
    digest = hashlib.sha256()
    total = 0
    with urllib.request.urlopen(request(filename), timeout=120) as response:
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > 1024 * 1024 * 1024:
                raise SystemExit(f"RTK asset exceeds the 1 GiB safety limit: {filename}")
            digest.update(chunk)
    if total == 0:
        raise SystemExit(f"RTK asset is empty: {filename}")
    actual = digest.hexdigest()
    if actual != expected[filename]:
        raise SystemExit(f"downloaded RTK asset does not match checksums.txt: {filename}")
    computed[architecture] = actual

print(f"{computed['amd64']}\t{computed['arm64']}")
PY
)"
IFS=$'\t' read -r latest_sha256_amd64 latest_sha256_arm64 <<<"${hashes}"
[[ "${latest_sha256_amd64}" =~ ^[0-9a-f]{64}$ ]] || die "invalid fetched RTK amd64 SHA-256"
[[ "${latest_sha256_arm64}" =~ ^[0-9a-f]{64}$ ]] || die "invalid fetched RTK arm64 SHA-256"

log "Updating RTK from ${current} to ${latest}"
python3 - "${DOCKERFILE}" "${latest}" "${latest_sha256_amd64}" "${latest_sha256_arm64}" <<'PY'
from pathlib import Path
import os
import re
import stat
import sys
import tempfile

path = Path(sys.argv[1])
latest, amd64_sha256, arm64_sha256 = sys.argv[2:]
text = path.read_text(encoding="utf-8")
replacements = (
    (r"^ARG RTK_VERSION=[^\s]+[ \t]*$", f"ARG RTK_VERSION={latest}", "RTK_VERSION"),
    (
        r'^([ \t]*rtk_sha256_amd64=")[^"]+(";.*)$',
        rf"\g<1>{amd64_sha256}\g<2>",
        "amd64 SHA-256",
    ),
    (
        r'^([ \t]*rtk_sha256_arm64=")[^"]+(";.*)$',
        rf"\g<1>{arm64_sha256}\g<2>",
        "arm64 SHA-256",
    ),
)

updated = text
for pattern, replacement, name in replacements:
    updated, count = re.subn(pattern, replacement, updated, count=1, flags=re.MULTILINE)
    if count != 1:
        raise SystemExit(f"expected to update exactly one {name} value, updated {count}")

mode = stat.S_IMODE(path.stat().st_mode)
temporary_path = None
try:
    with tempfile.NamedTemporaryFile(
        mode="w",
        encoding="utf-8",
        newline="",
        dir=path.parent,
        prefix=f".{path.name}.",
        suffix=".tmp",
        delete=False,
    ) as temporary:
        temporary.write(updated)
        temporary.flush()
        os.fsync(temporary.fileno())
        temporary_path = Path(temporary.name)
    os.chmod(temporary_path, mode)
    os.replace(temporary_path, path)
    temporary_path = None
finally:
    if temporary_path is not None:
        temporary_path.unlink(missing_ok=True)
PY

write_output changed true
