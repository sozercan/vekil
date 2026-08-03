#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
APP_PATH="${APP_PATH:-${REPO_ROOT}/Vekil.app}"
RESOLVED_MANIFEST="${MACOS_RESOLVED_MANIFEST:-${REPO_ROOT}/.build/macos/vekil-macos-release.json}"
OUTPUT_DIR="${MACOS_RELEASE_DIR:-${REPO_ROOT}/dist/macos-release}"
RELEASE_MODE="${MACOS_RELEASE:-0}"

[[ "$(uname -s)" == Darwin ]] || die "macOS app packaging requires Darwin"
for command in ditto shasum; do require_cmd "${command}"; done
[[ -d "${APP_PATH}" ]] || die "app bundle not found: ${APP_PATH}"
"${MANIFEST_TOOL}" validate --manifest "${RESOLVED_MANIFEST}"

artifact_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.artifact_name)"
manifest_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.release_manifest_name)"
artifact_path="${OUTPUT_DIR}/${artifact_name}"
sha_path="${artifact_path}.sha256"
external_manifest="${OUTPUT_DIR}/${manifest_name}"

mkdir -p "${OUTPUT_DIR}"
rm -f "${artifact_path}" "${sha_path}" "${external_manifest}"

if [[ "${RELEASE_MODE}" == 1 ]]; then
  submission_zip="${OUTPUT_DIR}/.${artifact_name%.zip}-notarization.zip"
  rm -f "${submission_zip}"
  log "Creating notarization submission from the signed app"
  ditto -c -k --sequesterRsrc --keepParent "${APP_PATH}" "${submission_zip}"
  "${SCRIPT_DIR}/notarize-macos-app.sh" "${APP_PATH}" "${submission_zip}"
  rm -f "${submission_zip}"
fi

log "Creating final immutable release ZIP"
ditto -c -k --sequesterRsrc --keepParent "${APP_PATH}" "${artifact_path}"
sha256="$(shasum -a 256 "${artifact_path}" | awk '{print $1}')"
printf '%s  %s\n' "${sha256}" "${artifact_name}" >"${sha_path}"
ditto "${RESOLVED_MANIFEST}" "${external_manifest}"

MACOS_RELEASE="${RELEASE_MODE}" \
MACOS_REQUIRE_NOTARIZATION="${RELEASE_MODE}" \
  "${SCRIPT_DIR}/verify-macos-release-artifact.sh" \
    "${artifact_path}" "${external_manifest}" "${sha_path}" >/dev/null

log "Packaged ${artifact_path}"
printf '%s\n' "${artifact_path}"
