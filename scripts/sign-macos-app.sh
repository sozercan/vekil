#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
APP_PATH="${APP_PATH:-${REPO_ROOT}/Vekil.app}"
IDENTITY="${MACOS_SIGNING_IDENTITY:--}"
RELEASE_MODE="${MACOS_RELEASE:-0}"
ENTITLEMENTS="${MACOS_APP_ENTITLEMENTS:-}"

[[ "$(uname -s)" == Darwin ]] || die "macOS code signing requires Darwin"
require_cmd codesign
require_cmd security

app_executable="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.executable)"
helper_executable="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.helper_executable)"
framework_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.framework_path)"
framework="${APP_PATH}/Contents/Frameworks/${framework_rel}"

[[ -d "${APP_PATH}" ]] || die "app bundle not found: ${APP_PATH}"
[[ -x "${APP_PATH}/Contents/MacOS/${app_executable}" ]] || die "app executable not found"
[[ -x "${APP_PATH}/Contents/Helpers/${helper_executable}" ]] || die "helper executable not found"
[[ -d "${framework}" ]] || die "Sparkle framework not found"

if [[ "${RELEASE_MODE}" == 1 ]]; then
  [[ -n "${IDENTITY}" && "${IDENTITY}" != "-" ]] || die "release signing requires MACOS_SIGNING_IDENTITY"
  security find-identity -v -p codesigning | grep -Fq "${IDENTITY}" || die "signing identity is not available: ${IDENTITY}"
fi
if [[ -n "${ENTITLEMENTS}" && ! -f "${ENTITLEMENTS}" ]]; then
  die "entitlements file not found: ${ENTITLEMENTS}"
fi

sign_args=(--force --sign "${IDENTITY}")
if [[ "${IDENTITY}" == "-" ]]; then
  # Hardened-runtime library validation treats independently ad-hoc-signed
  # embedded frameworks as different teams. Local development signatures do
  # not claim the production boundary, so omit the runtime flag and exercise
  # the exact bundle with both architectures in the smoke tests instead.
  sign_args+=(--timestamp=none)
else
  sign_args+=(--options runtime --timestamp)
fi

sign_plain() {
  local target="$1"
  [[ -e "${target}" ]] || die "code-sign target not found: ${target}"
  log "Signing ${target#"${APP_PATH}"/}"
  codesign "${sign_args[@]}" "${target}"
}

sign_preserving_metadata() {
  local target="$1"
  [[ -e "${target}" ]] || die "nested Sparkle code not found: ${target}"
  log "Re-signing ${target#"${APP_PATH}"/} while preserving Sparkle metadata"
  codesign "${sign_args[@]}" \
    --preserve-metadata=identifier,entitlements,requirements,flags,runtime \
    "${target}"
}

# Sign Sparkle from the deepest nested code outward. These paths are part of the
# pinned Sparkle 2 framework layout and are verified again after signing.
sign_preserving_metadata "${framework}/Versions/B/Autoupdate"
sign_preserving_metadata "${framework}/Versions/B/XPCServices/Downloader.xpc"
sign_preserving_metadata "${framework}/Versions/B/XPCServices/Installer.xpc"
sign_preserving_metadata "${framework}/Versions/B/Updater.app"
sign_preserving_metadata "${framework}"

sign_plain "${APP_PATH}/Contents/Helpers/${helper_executable}"
sign_plain "${APP_PATH}/Contents/MacOS/${app_executable}"

app_sign_args=("${sign_args[@]}")
if [[ -n "${ENTITLEMENTS}" ]]; then
  app_sign_args+=(--entitlements "${ENTITLEMENTS}")
fi
log "Signing ${APP_PATH}"
codesign "${app_sign_args[@]}" "${APP_PATH}"

codesign --verify --deep --strict --verbose=2 "${APP_PATH}"
log "Code signing verification passed"
