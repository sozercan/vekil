#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

[[ $# -ge 2 && $# -le 3 ]] || die "usage: $0 <release-zip> <release-manifest.json> [sha256-file]"
ZIP_PATH="$1"
RESOLVED_MANIFEST="$2"
SHA_FILE="${3:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"

[[ "$(uname -s)" == Darwin ]] || die "release ZIP verification requires Darwin"
for command in ditto shasum python3; do require_cmd "${command}"; done
[[ -f "${ZIP_PATH}" ]] || die "release ZIP not found: ${ZIP_PATH}"
[[ -f "${RESOLVED_MANIFEST}" ]] || die "release manifest not found: ${RESOLVED_MANIFEST}"
"${MANIFEST_TOOL}" validate --manifest "${RESOLVED_MANIFEST}"

artifact_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.artifact_name)"
bundle_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.bundle_name)"
[[ "$(basename "${ZIP_PATH}")" == "${artifact_name}" ]] || die "artifact must be named ${artifact_name}"
actual_sha256="$(shasum -a 256 "${ZIP_PATH}" | awk '{print $1}')"

if [[ -n "${SHA_FILE}" ]]; then
  [[ -f "${SHA_FILE}" ]] || die "SHA-256 file not found: ${SHA_FILE}"
  read -r expected_sha256 expected_name <"${SHA_FILE}"
  expected_name="${expected_name#\*}"
  [[ "${expected_name}" == "${artifact_name}" ]] || die "SHA-256 file names ${expected_name}; expected ${artifact_name}"
  [[ "${actual_sha256}" == "${expected_sha256}" ]] || die "release ZIP SHA-256 mismatch"
fi
if [[ -n "${EXPECTED_MACOS_ZIP_SHA256:-}" && "${actual_sha256}" != "${EXPECTED_MACOS_ZIP_SHA256}" ]]; then
  die "release ZIP does not match EXPECTED_MACOS_ZIP_SHA256"
fi

extract_root="$(mktemp -d "${TMPDIR:-/tmp}/vekil-release-verify.XXXXXX")"
trap 'rm -rf "${extract_root}"' EXIT
ditto -x -k "${ZIP_PATH}" "${extract_root}"
app_path="${extract_root}/${bundle_name}"
[[ -d "${app_path}" ]] || die "archive does not contain ${bundle_name} at its root"

python3 - "${extract_root}" "${bundle_name}" <<'PY'
import sys
from pathlib import Path

root = Path(sys.argv[1])
expected = sys.argv[2]
entries = sorted(path.name for path in root.iterdir() if path.name != "__MACOSX")
if entries != [expected]:
    raise SystemExit(f"release ZIP has unexpected root entries: {entries}")
PY

APP_PATH="${app_path}" \
MACOS_APP_CONFIG="${CONFIG}" \
MACOS_RESOLVED_MANIFEST="${RESOLVED_MANIFEST}" \
MACOS_RELEASE="${MACOS_RELEASE:-0}" \
MACOS_REQUIRE_NOTARIZATION="${MACOS_REQUIRE_NOTARIZATION:-0}" \
  "${SCRIPT_DIR}/verify-macos-app.sh"

log "Verified exact release ZIP ${artifact_name}"
printf '%s\n' "${actual_sha256}"
