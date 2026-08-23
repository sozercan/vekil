#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

[[ $# -eq 3 ]] || die "usage: $0 <release-zip> <release-manifest.json> <output-appcast.xml>"
ZIP_PATH="$1"
RESOLVED_MANIFEST="$2"
OUTPUT_APPCAST="$3"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"

[[ "$(uname -s)" == Darwin ]] || die "Sparkle appcast generation requires Darwin"
for command in curl ditto openssl shasum python3; do require_cmd "${command}"; done
[[ -f "${ZIP_PATH}" ]] || die "release ZIP not found: ${ZIP_PATH}"
[[ -f "${RESOLVED_MANIFEST}" ]] || die "release manifest not found: ${RESOLVED_MANIFEST}"
[[ -n "${SPARKLE_PRIVATE_ED_KEY:-}" ]] || die "SPARKLE_PRIVATE_ED_KEY is required to sign the appcast"
"${MANIFEST_TOOL}" validate --manifest "${RESOLVED_MANIFEST}"

artifact_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.artifact_name)"
appcast_tool_rel="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.generate_appcast_path)"
default_base_appcast="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.feed_url)"
base_appcast_url="${SPARKLE_BASE_APPCAST_URL:-${default_base_appcast}}"
base_appcast_file="${SPARKLE_BASE_APPCAST_FILE:-}"
require_base_appcast="${SPARKLE_REQUIRE_BASE_APPCAST:-${MACOS_RELEASE:-0}}"
legacy_artifact_file="${SPARKLE_LEGACY_ARTIFACT_FILE:-}"
download_url_prefix="${SPARKLE_DOWNLOAD_URL_PREFIX:-}"
release_notes_url="${SPARKLE_RELEASE_NOTES_URL:-}"

[[ "$(basename "${ZIP_PATH}")" == "${artifact_name}" ]] || die "release ZIP must be named ${artifact_name}"
[[ -n "${download_url_prefix}" ]] || die "SPARKLE_DOWNLOAD_URL_PREFIX is required"
[[ "${download_url_prefix}" == https://* ]] || die "SPARKLE_DOWNLOAD_URL_PREFIX must use HTTPS"
[[ -n "${release_notes_url}" ]] || die "SPARKLE_RELEASE_NOTES_URL is required"
[[ "${release_notes_url}" == https://* ]] || die "SPARKLE_RELEASE_NOTES_URL must use HTTPS"

sparkle_root="$(MACOS_APP_CONFIG="${CONFIG}" "${SCRIPT_DIR}/fetch-sparkle.sh")"
appcast_tool="${sparkle_root}/${appcast_tool_rel}"
[[ -x "${appcast_tool}" ]] || die "verified generate_appcast tool not found: ${appcast_tool}"

before_sha256="$(shasum -a 256 "${ZIP_PATH}" | awk '{print $1}')"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-appcast.XXXXXX")"
legacy_work_dir="$(mktemp -d "${TMPDIR:-/tmp}/vekil-legacy-artifact.XXXXXX")"
trap 'rm -rf "${work_dir}" "${legacy_work_dir}"' EXIT
ditto "${ZIP_PATH}" "${work_dir}/${artifact_name}"

if [[ -n "${base_appcast_file}" ]]; then
  [[ -f "${base_appcast_file}" ]] || die "SPARKLE_BASE_APPCAST_FILE not found: ${base_appcast_file}"
  ditto "${base_appcast_file}" "${work_dir}/appcast.xml"
elif [[ -n "${base_appcast_url}" ]]; then
  if ! curl \
      --fail --silent --show-error --location \
      --proto '=https' --tlsv1.2 \
      --retry 4 --retry-all-errors --connect-timeout 15 --max-time 120 \
      --output "${work_dir}/appcast.xml" \
      "${base_appcast_url}"; then
    if [[ "${require_base_appcast}" == 1 ]]; then
      die "failed to download required base appcast: ${base_appcast_url}"
    fi
    rm -f "${work_dir}/appcast.xml"
    log "No base appcast was available; generating a candidate-only test feed"
  fi
fi

legacy_artifact_path=""
if [[ "${require_base_appcast}" == 1 ]]; then
  legacy_artifact_url="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key legacy_shell.last_compatible_artifact_url)"
  legacy_artifact_sha256="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key legacy_shell.last_compatible_artifact_sha256)"
  if [[ -n "${legacy_artifact_file}" ]]; then
    [[ -f "${legacy_artifact_file}" ]] || die "SPARKLE_LEGACY_ARTIFACT_FILE not found: ${legacy_artifact_file}"
    legacy_artifact_path="${legacy_artifact_file}"
  else
    legacy_artifact_path="${legacy_work_dir}/legacy-compatible-artifact.zip"
    curl \
      --fail --silent --show-error --location \
      --proto '=https' --tlsv1.2 \
      --retry 4 --retry-all-errors --connect-timeout 15 --max-time 120 \
      --output "${legacy_artifact_path}" \
      "${legacy_artifact_url}" || die "failed to download pinned legacy artifact: ${legacy_artifact_url}"
  fi
  actual_legacy_sha256="$(shasum -a 256 "${legacy_artifact_path}" | awk '{print $1}')"
  [[ "${actual_legacy_sha256}" == "${legacy_artifact_sha256}" ]] || \
    die "legacy artifact SHA-256 does not match the release manifest"
fi

log "Running checksum-verified Sparkle generate_appcast"
printf '%s' "${SPARKLE_PRIVATE_ED_KEY}" | "${appcast_tool}" \
  --ed-key-file - \
  --download-url-prefix "${download_url_prefix%/}/" \
  --full-release-notes-url "${release_notes_url}" \
  --maximum-deltas 0 \
  --maximum-versions 0 \
  "${work_dir}"

[[ -f "${work_dir}/appcast.xml" ]] || die "generate_appcast did not produce appcast.xml"
mkdir -p "$(dirname "${OUTPUT_APPCAST}")"
ditto "${work_dir}/appcast.xml" "${OUTPUT_APPCAST}"

after_sha256="$(shasum -a 256 "${ZIP_PATH}" | awk '{print $1}')"
[[ "${before_sha256}" == "${after_sha256}" ]] || die "release ZIP changed during appcast generation"

verify_args=(
  --appcast "${OUTPUT_APPCAST}"
  --manifest "${RESOLVED_MANIFEST}"
  --artifact "${ZIP_PATH}"
  --expected-url-prefix "${download_url_prefix%/}"
)
if [[ "${require_base_appcast}" == 1 ]]; then
  verify_args+=(
    --require-legacy-compatible-entry
    --legacy-artifact "${legacy_artifact_path}"
  )
fi
"${SCRIPT_DIR}/verify-sparkle-appcast.py" "${verify_args[@]}"
log "Generated and verified ${OUTPUT_APPCAST} without modifying the release ZIP"
