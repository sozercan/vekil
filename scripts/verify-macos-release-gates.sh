#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[[ $# -eq 2 ]] || die "usage: $0 <release-zip> <sha256-file>"
ZIP_PATH="$1"
SHA_FILE="$2"
[[ -f "${ZIP_PATH}" ]] || die "release ZIP not found: ${ZIP_PATH}"
[[ -f "${SHA_FILE}" ]] || die "SHA-256 file not found: ${SHA_FILE}"
[[ "${MACOS_RELEASE:-0}" == 1 ]] || die "release gates require MACOS_RELEASE=1"

actual_sha256="$(shasum -a 256 "${ZIP_PATH}" | awk '{print $1}')"
read -r recorded_sha256 recorded_name <"${SHA_FILE}"
recorded_name="${recorded_name#\*}"
[[ "${recorded_sha256}" == "${actual_sha256}" ]] || die "recorded SHA-256 does not match the candidate ZIP"
[[ "${recorded_name}" == "$(basename "${ZIP_PATH}")" ]] || die "SHA-256 file names the wrong artifact"

require_digest_gate() {
  local variable="$1" description="$2" value
  value="${!variable:-}"
  [[ -n "${value}" ]] || die "${variable} is required: ${description}"
  [[ "${value}" == "${actual_sha256}" ]] || die "${variable} does not match candidate SHA-256 ${actual_sha256}"
  log "Gate passed: ${description}"
}

# These values are environment-scoped release attestations. They must be set to
# the exact candidate digest only after the corresponding external test has run.
require_digest_gate MACOS_N_MINUS_ONE_UPDATE_TESTED_SHA256 \
  "a released Go-shell build updated through Sparkle to this exact native ZIP"
require_digest_gate MACOS_HOMEBREW_INSTALL_TESTED_SHA256 \
  "the generated macOS 13+ Homebrew cask installed this exact universal ZIP"
require_digest_gate MACOS_FORWARD_REVERT_TESTED_SHA256 \
  "a higher-CFBundleVersion forward-revert recovered from this exact candidate"

if [[ "${MACOS_MANAGED_CONFIG_SHIPPING:-0}" == 1 ]]; then
  require_digest_gate MACOS_KEYCHAIN_CONTINUITY_TESTED_SHA256 \
    "Keychain create/read/update/delete continuity passed across an A-to-B Sparkle update"
else
  [[ -z "${MACOS_KEYCHAIN_CONTINUITY_TESTED_SHA256:-}" || "${MACOS_KEYCHAIN_CONTINUITY_TESTED_SHA256}" == "${actual_sha256}" ]] || \
    die "MACOS_KEYCHAIN_CONTINUITY_TESTED_SHA256 is set but does not match this candidate"
  log "Managed configuration is not shipping; Keychain update continuity remains a future release gate"
fi

printf '%s\n' "${actual_sha256}"
