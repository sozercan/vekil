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
RESOLVED_MANIFEST="${MACOS_RESOLVED_MANIFEST:-${REPO_ROOT}/.build/macos/vekil-macos-release.json}"
RELEASE_MODE="${MACOS_RELEASE:-0}"
REQUIRE_NOTARIZATION="${MACOS_REQUIRE_NOTARIZATION:-0}"

[[ "$(uname -s)" == Darwin ]] || die "Mach-O app verification requires Darwin"
for command in codesign lipo otool plutil python3 xcrun; do
  require_cmd "${command}"
done
[[ -x /usr/libexec/PlistBuddy ]] || die "PlistBuddy is unavailable"

"${MANIFEST_TOOL}" validate-config --config "${CONFIG}"
"${MANIFEST_TOOL}" validate --manifest "${RESOLVED_MANIFEST}"

app_bundle_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.bundle_name)"
bundle_identifier="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.bundle_identifier)"
app_executable="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.executable)"
helper_executable="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.helper_executable)"
minimum_system_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.minimum_system_version)"
release_manifest_name="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key application.release_manifest_name)"
marketing_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key marketing_version)"
bundle_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key bundle_version)"
bundle_build_id="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key bundle_build_id)"
framework_rel="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.framework_path)"
sparkle_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.version)"
sparkle_feed="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.feed_url)"
sparkle_public_key="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key sparkle.public_ed_key)"

[[ "$(basename "${APP_PATH}")" == "${app_bundle_name}" ]] || die "unexpected app bundle name: ${APP_PATH}"
plist="${APP_PATH}/Contents/Info.plist"
app_binary="${APP_PATH}/Contents/MacOS/${app_executable}"
helper_binary="${APP_PATH}/Contents/Helpers/${helper_executable}"
framework="${APP_PATH}/Contents/Frameworks/${framework_rel}"
embedded_manifest="${APP_PATH}/Contents/Resources/${release_manifest_name}"

[[ -f "${plist}" ]] || die "Info.plist not found"
[[ -x "${app_binary}" ]] || die "native app executable not found: ${app_binary}"
[[ -x "${helper_binary}" ]] || die "runtime helper not found: ${helper_binary}"
[[ -d "${framework}" ]] || die "Sparkle framework not found: ${framework}"
[[ -f "${APP_PATH}/Contents/Resources/Vekil.icns" ]] || die "Vekil.icns not found"
[[ -f "${embedded_manifest}" ]] || die "embedded release manifest not found"
cmp -s "${RESOLVED_MANIFEST}" "${embedded_manifest}" || die "embedded release manifest differs from build manifest"
plutil -lint "${plist}" >/dev/null

plist_value() {
  /usr/libexec/PlistBuddy -c "Print :$1" "${plist}" 2>/dev/null
}
require_plist_value() {
  local key="$1" expected="$2" actual
  actual="$(plist_value "${key}")" || die "Info.plist is missing ${key}"
  [[ "${actual}" == "${expected}" ]] || die "Info.plist ${key} is ${actual}; expected ${expected}"
}

require_plist_value CFBundleExecutable "${app_executable}"
require_plist_value CFBundleIdentifier "${bundle_identifier}"
require_plist_value CFBundleShortVersionString "${marketing_version}"
require_plist_value CFBundleVersion "${bundle_version}"
require_plist_value LSMinimumSystemVersion "${minimum_system_version}"
require_plist_value LSUIElement true
require_plist_value SUFeedURL "${sparkle_feed}"
require_plist_value SUPublicEDKey "${sparkle_public_key}"
require_plist_value SURequireSignedFeed true
require_plist_value SUVerifyUpdateBeforeExtraction true
require_plist_value VekilBundleBuildID "${bundle_build_id}"
require_plist_value VekilReleaseManifest "${release_manifest_name}"
if plist_value SUEnableInstallerLauncherService >/dev/null 2>&1; then
  die "native app must omit SUEnableInstallerLauncherService"
fi

python3 - "${framework}" "${sparkle_version}" <<'PY'
import plistlib
import sys
from pathlib import Path

framework = Path(sys.argv[1])
expected = sys.argv[2]
for path in (
    framework / "Versions/B/Resources/Info.plist",
    framework / "Resources/Info.plist",
):
    if path.exists():
        with path.open("rb") as handle:
            actual = plistlib.load(handle).get("CFBundleShortVersionString")
        if actual != expected:
            raise SystemExit(f"Sparkle framework version {actual!r}; expected {expected!r}")
        break
else:
    raise SystemExit("Sparkle framework Info.plist not found")
PY

python3 - "${APP_PATH}" <<'PY'
import os
import sys
from pathlib import Path

root = Path(sys.argv[1]).resolve()
for path in root.rglob("*"):
    if path.is_symlink():
        resolved = path.resolve(strict=False)
        try:
            resolved.relative_to(root)
        except ValueError:
            raise SystemExit(f"symlink escapes app bundle: {path} -> {os.readlink(path)}")
PY

require_universal() {
  local binary="$1" label="$2" archs
  [[ -f "${binary}" ]] || die "${label} Mach-O not found: ${binary}"
  archs="$(lipo -archs "${binary}" 2>/dev/null)" || die "${label} is not a universal Mach-O: ${binary}"
  case " ${archs} " in *" arm64 "*) ;; *) die "${label} is missing arm64: ${archs}" ;; esac
  case " ${archs} " in *" x86_64 "*) ;; *) die "${label} is missing x86_64: ${archs}" ;; esac
  extra="$(tr ' ' '\n' <<<"${archs}" | grep -Ev '^(arm64|x86_64)$' || true)"
  [[ -z "${extra}" ]] || die "${label} has unexpected architectures: ${archs}"
}

require_macos_minimum() {
  local binary="$1" label="$2" arch output actual
  for arch in arm64 x86_64; do
    output="$(xcrun vtool -arch "${arch}" -show-build "${binary}" 2>&1)" || die "cannot inspect ${label} ${arch} LC_BUILD_VERSION: ${output}"
    actual="$(awk '$1 == "minos" { print $2; exit }' <<<"${output}")"
    [[ -n "${actual}" ]] || die "${label} ${arch} has no LC_BUILD_VERSION minos"
    if [[ "$("${MANIFEST_TOOL}" compare-system-versions "${actual}" "${minimum_system_version}")" != 0 ]]; then
      die "${label} ${arch} minos is ${actual}; expected ${minimum_system_version}"
    fi
  done
}

require_universal "${app_binary}" "app executable"
require_universal "${helper_binary}" "runtime helper"
require_macos_minimum "${app_binary}" "app executable"
require_macos_minimum "${helper_binary}" "runtime helper"

sparkle_machos=(
  "${framework}/Versions/B/Sparkle"
  "${framework}/Versions/B/Autoupdate"
  "${framework}/Versions/B/Updater.app/Contents/MacOS/Updater"
  "${framework}/Versions/B/XPCServices/Downloader.xpc/Contents/MacOS/Downloader"
  "${framework}/Versions/B/XPCServices/Installer.xpc/Contents/MacOS/Installer"
)
for binary in "${sparkle_machos[@]}"; do
  require_universal "${binary}" "nested Sparkle component"
done

rpaths="$(otool -l "${app_binary}" | awk '/cmd LC_RPATH/{seen=1; next} seen && $1=="path" {print $2; seen=0}')"
grep -Fxq '@executable_path/../Frameworks' <<<"${rpaths}" || die "app executable is missing @executable_path/../Frameworks rpath"

check_dependencies() {
  local binary="$1" label="$2" dependency
  while IFS= read -r dependency; do
    [[ -n "${dependency}" ]] || continue
    case "${dependency}" in
      /System/Library/*|/usr/lib/*|@rpath/*|@loader_path/*|@executable_path/*) ;;
      *) die "${label} has non-system absolute dependency: ${dependency}" ;;
    esac
    case "${dependency}" in
      *"/.build/"*|*"/opt/homebrew/"*|*"/usr/local/"*|*"${REPO_ROOT}"*)
        die "${label} leaks a build-machine dependency: ${dependency}"
        ;;
    esac
  done < <(otool -L "${binary}" | awk '/^[[:space:]]/ {print $1}')
}
check_dependencies "${app_binary}" "app executable"
check_dependencies "${helper_binary}" "runtime helper"
otool -L "${app_binary}" | grep -Fq '@rpath/Sparkle.framework/Versions/B/Sparkle' || die "app executable is not linked to Sparkle.framework"
if otool -L "${helper_binary}" | grep -Fq 'Sparkle.framework'; then
  die "runtime helper must not link Sparkle"
fi

codesign --verify --deep --strict --verbose=2 "${APP_PATH}"
codesign --verify --strict --verbose=2 "${app_binary}"
codesign --verify --strict --verbose=2 "${helper_binary}"

entitlements="$(codesign -d --entitlements :- "${APP_PATH}" 2>/dev/null || true)"
if grep -A1 -E '<key>com\.apple\.security\.get-task-allow</key>' <<<"${entitlements}" | grep -q '<true/>'; then
  die "release app must not include get-task-allow entitlement"
fi
if grep -A1 -E '<key>com\.apple\.security\.app-sandbox</key>' <<<"${entitlements}" | grep -q '<true/>'; then
  die "sandboxing is not enabled for this release pipeline"
fi

if [[ "${RELEASE_MODE}" == 1 ]]; then
  app_signature_details="$(codesign -dv --verbose=4 "${APP_PATH}" 2>&1)"
  team_identifier="$(awk -F= '/^TeamIdentifier=/{print $2; exit}' <<<"${app_signature_details}")"
  [[ -n "${team_identifier}" && "${team_identifier}" != "not set" ]] || die "release signature has no TeamIdentifier"
  if [[ -n "${MACOS_EXPECTED_TEAM_ID:-}" && "${team_identifier}" != "${MACOS_EXPECTED_TEAM_ID}" ]]; then
    die "release TeamIdentifier ${team_identifier} does not match MACOS_EXPECTED_TEAM_ID"
  fi

  signature_targets=(
    "${APP_PATH}"
    "${app_binary}"
    "${helper_binary}"
    "${framework}"
    "${framework}/Versions/B/Autoupdate"
    "${framework}/Versions/B/Updater.app"
    "${framework}/Versions/B/XPCServices/Downloader.xpc"
    "${framework}/Versions/B/XPCServices/Installer.xpc"
  )
  for target in "${signature_targets[@]}"; do
    codesign --verify --strict --verbose=2 "${target}"
    signature_details="$(codesign -dv --verbose=4 "${target}" 2>&1)"
    grep -Fq 'Authority=Developer ID Application:' <<<"${signature_details}" || die "release code is not Developer ID signed: ${target}"
    grep -Eq '^Timestamp=' <<<"${signature_details}" || die "release signature has no secure timestamp: ${target}"
    grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"${signature_details}" || die "release signature lacks hardened runtime: ${target}"
    if grep -Fq 'Signature=adhoc' <<<"${signature_details}"; then
      die "release code has an ad-hoc signature: ${target}"
    fi
    target_team="$(awk -F= '/^TeamIdentifier=/{print $2; exit}' <<<"${signature_details}")"
    [[ "${target_team}" == "${team_identifier}" ]] || die "release code has mixed TeamIdentifiers: ${target}"
  done
fi

if [[ "${REQUIRE_NOTARIZATION}" == 1 ]]; then
  require_cmd spctl
  xcrun stapler validate "${APP_PATH}"
  spctl --assess --type execute --verbose=4 "${APP_PATH}"
fi

log "Verified universal native app, helper, Sparkle ${sparkle_version}, macOS ${minimum_system_version} metadata, dependencies, and signatures"
