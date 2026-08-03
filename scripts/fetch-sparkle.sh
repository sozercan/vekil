#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
CACHE_ROOT="${SPARKLE_CACHE_DIR:-${REPO_ROOT}/.build/sparkle}"
DOWNLOAD_ROOT="${SPARKLE_DOWNLOAD_DIR:-${REPO_ROOT}/.build/downloads}"

require_cmd curl
require_cmd python3
require_cmd tar
require_cmd shasum

"${MANIFEST_TOOL}" validate-config --config "${CONFIG}"
version="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.version)"
archive_name="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.archive_name)"
archive_url="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.archive_url)"
archive_sha256="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.archive_sha256)"
framework_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.framework_path)"
appcast_tool_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.generate_appcast_path)"
archive_path="${DOWNLOAD_ROOT}/${archive_name}"
unpack_dir="${CACHE_ROOT}/${version}"
marker="${unpack_dir}/.verified-sha256"

verify_archive() {
  [[ -f "${archive_path}" ]] || return 1
  local actual
  actual="$(shasum -a 256 "${archive_path}" | awk '{print $1}')"
  [[ "${actual}" == "${archive_sha256}" ]]
}

verify_unpacked() {
  [[ -f "${marker}" ]] || return 1
  [[ "$(cat "${marker}")" == "${archive_sha256}" ]] || return 1
  [[ -d "${unpack_dir}/${framework_rel}" ]] || return 1
  [[ -x "${unpack_dir}/${appcast_tool_rel}" ]] || return 1
  python3 - "${unpack_dir}/${framework_rel}" "${version}" <<'PY'
import plistlib
import sys
from pathlib import Path

framework = Path(sys.argv[1])
expected = sys.argv[2]
candidates = [
    framework / "Versions/B/Resources/Info.plist",
    framework / "Resources/Info.plist",
]
for candidate in candidates:
    if candidate.exists():
        with candidate.open("rb") as handle:
            value = plistlib.load(handle).get("CFBundleShortVersionString")
        if value != expected:
            raise SystemExit(f"Sparkle framework version {value!r} != {expected!r}")
        raise SystemExit(0)
raise SystemExit("Sparkle framework Info.plist not found")
PY
}

mkdir -p "${DOWNLOAD_ROOT}" "${CACHE_ROOT}"

if ! verify_archive; then
  if [[ -e "${archive_path}" ]]; then
    log "Discarding cached Sparkle archive with an invalid SHA-256"
    rm -f "${archive_path}"
  fi
  temp_archive="$(mktemp "${DOWNLOAD_ROOT}/.${archive_name}.XXXXXX")"
  trap 'rm -f "${temp_archive:-}"' EXIT
  log "Downloading Sparkle ${version} from the pinned HTTPS URL"
  curl \
    --fail --silent --show-error --location \
    --proto '=https' --tlsv1.2 \
    --retry 4 --retry-all-errors --connect-timeout 15 --max-time 300 \
    --output "${temp_archive}" \
    "${archive_url}"
  actual="$(shasum -a 256 "${temp_archive}" | awk '{print $1}')"
  [[ "${actual}" == "${archive_sha256}" ]] || die "Sparkle SHA-256 mismatch: expected ${archive_sha256}, got ${actual}"
  mv "${temp_archive}" "${archive_path}"
  temp_archive=""
fi

verify_archive || die "cached Sparkle archive failed SHA-256 verification"

if ! verify_unpacked; then
  temp_unpack="$(mktemp -d "${CACHE_ROOT}/.${version}.XXXXXX")"
  trap 'rm -f "${temp_archive:-}"; rm -rf "${temp_unpack:-}"' EXIT
  log "Validating Sparkle archive paths before extraction"
  python3 - "${archive_path}" <<'PY'
import os
import sys
import tarfile
from pathlib import PurePosixPath

archive = sys.argv[1]
with tarfile.open(archive, mode="r:xz") as handle:
    for member in handle.getmembers():
        name = member.name
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts:
            raise SystemExit(f"unsafe archive path: {name}")
        if member.issym() or member.islnk():
            link = PurePosixPath(member.linkname)
            if link.is_absolute():
                raise SystemExit(f"unsafe absolute link: {name} -> {member.linkname}")
            resolved = PurePosixPath(os.path.normpath(str(path.parent / link)))
            if ".." in resolved.parts:
                raise SystemExit(f"unsafe escaping link: {name} -> {member.linkname}")
PY
  tar -xf "${archive_path}" -C "${temp_unpack}"
  printf '%s\n' "${archive_sha256}" >"${temp_unpack}/.verified-sha256"
  rm -rf "${unpack_dir}"
  mv "${temp_unpack}" "${unpack_dir}"
  temp_unpack=""
fi

verify_unpacked || die "Sparkle ${version} extraction failed verification"
log "Verified Sparkle ${version} (${archive_sha256})"
printf '%s\n' "${unpack_dir}"
