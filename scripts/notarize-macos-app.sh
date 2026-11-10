#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

[[ $# -eq 2 ]] || die "usage: $0 <signed-app-path> <submission-zip>"
APP_PATH="$1"
SUBMISSION_ZIP="$2"

[[ "$(uname -s)" == Darwin ]] || die "notarization requires Darwin"
[[ "${MACOS_RELEASE:-0}" == 1 ]] || die "notarization is a release-only gate; set MACOS_RELEASE=1"
[[ -d "${APP_PATH}" ]] || die "app bundle not found: ${APP_PATH}"
[[ -f "${SUBMISSION_ZIP}" ]] || die "notarization ZIP not found: ${SUBMISSION_ZIP}"
require_cmd xcrun
require_cmd spctl

notary_args=()
if [[ -n "${MACOS_NOTARY_KEYCHAIN_PROFILE:-}" ]]; then
  notary_args+=(--keychain-profile "${MACOS_NOTARY_KEYCHAIN_PROFILE}")
elif [[ -n "${APPLE_API_KEY_PATH:-}" || -n "${APPLE_API_KEY_ID:-}" || -n "${APPLE_API_ISSUER_ID:-}" ]]; then
  [[ -f "${APPLE_API_KEY_PATH:-}" ]] || die "APPLE_API_KEY_PATH must name the App Store Connect API private-key file"
  [[ -n "${APPLE_API_KEY_ID:-}" ]] || die "APPLE_API_KEY_ID is required"
  [[ -n "${APPLE_API_ISSUER_ID:-}" ]] || die "APPLE_API_ISSUER_ID is required"
  notary_args+=(--key "${APPLE_API_KEY_PATH}" --key-id "${APPLE_API_KEY_ID}" --issuer "${APPLE_API_ISSUER_ID}")
elif [[ -n "${APPLE_ID:-}" || -n "${APPLE_TEAM_ID:-}" || -n "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]]; then
  [[ -n "${APPLE_ID:-}" ]] || die "APPLE_ID is required"
  [[ -n "${APPLE_TEAM_ID:-}" ]] || die "APPLE_TEAM_ID is required"
  [[ -n "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]] || die "APPLE_APP_SPECIFIC_PASSWORD is required"
  notary_args+=(--apple-id "${APPLE_ID}" --team-id "${APPLE_TEAM_ID}" --password "${APPLE_APP_SPECIFIC_PASSWORD}")
else
  die "configure MACOS_NOTARY_KEYCHAIN_PROFILE, App Store Connect API credentials, or Apple ID notarization credentials"
fi

log "Submitting signed app for notarization"
xcrun notarytool submit "${SUBMISSION_ZIP}" --wait "${notary_args[@]}"
log "Stapling notarization ticket"
xcrun stapler staple "${APP_PATH}"
xcrun stapler validate "${APP_PATH}"
spctl --assess --type execute --verbose=4 "${APP_PATH}"
log "Notarization, staple, and Gatekeeper assessment passed"
