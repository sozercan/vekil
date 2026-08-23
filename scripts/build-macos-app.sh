#!/usr/bin/env bash

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CONFIG="${MACOS_APP_CONFIG:-${REPO_ROOT}/build-support/macos/app-config.json}"
MANIFEST_TOOL="${SCRIPT_DIR}/macos-release-manifest.py"
BUILD_ROOT="${MACOS_BUILD_ROOT:-${REPO_ROOT}/.build/macos}"
APP_PATH="${APP_PATH:-${REPO_ROOT}/Vekil.app}"
RESOLVED_MANIFEST="${MACOS_RESOLVED_MANIFEST:-${BUILD_ROOT}/vekil-macos-release.json}"
MARKETING_VERSION="${VERSION:-}"
BUNDLE_VERSION="${MACOS_BUNDLE_VERSION:-}"
BUNDLE_BUILD_ID="${MACOS_BUNDLE_BUILD_ID:-}"
RELEASE_MODE="${MACOS_RELEASE:-0}"

[[ "$(uname -s)" == Darwin ]] || die "native macOS app assembly requires Darwin"
for command in swift go clang lipo ditto python3; do
  require_cmd "${command}"
done

"${SCRIPT_DIR}/macos-native-source-status.sh" --require
"${MANIFEST_TOOL}" validate-config --config "${CONFIG}"

app_name="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.name)"
app_bundle_name="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.bundle_name)"
app_executable="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.executable)"
helper_executable="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.helper_executable)"
minimum_system_version="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.minimum_system_version)"
icon_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.icon_path)"
swift_package_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.swift_package_path)"
swift_product="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.swift_product)"
go_helper_package="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.go_helper_package)"
release_manifest_name="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key application.release_manifest_name)"
framework_rel="$(${MANIFEST_TOOL} get --file "${CONFIG}" --key sparkle.framework_path)"

[[ "$(basename "${APP_PATH}")" == "${app_bundle_name}" ]] || die "APP_PATH must end in ${app_bundle_name}"
[[ -f "${REPO_ROOT}/${icon_rel}" ]] || die "icon not found: ${icon_rel}"

resolve_args=(
  resolve
  --config "${CONFIG}"
  --output "${RESOLVED_MANIFEST}"
)
[[ -z "${MARKETING_VERSION}" ]] || resolve_args+=(--marketing-version "${MARKETING_VERSION}")
[[ -z "${BUNDLE_VERSION}" ]] || resolve_args+=(--bundle-version "${BUNDLE_VERSION}")
[[ -z "${BUNDLE_BUILD_ID}" ]] || resolve_args+=(--bundle-build-id "${BUNDLE_BUILD_ID}")
if [[ "${RELEASE_MODE}" == 1 ]]; then
  resolve_args+=(--release)
fi
"${MANIFEST_TOOL}" "${resolve_args[@]}"

marketing_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key marketing_version)"
bundle_version="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key bundle_version)"
bundle_build_id="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key bundle_build_id)"
source_commit="$(${MANIFEST_TOOL} get --file "${RESOLVED_MANIFEST}" --key source_commit)"

log "Building ${app_name} ${marketing_version} (${bundle_version}, ${bundle_build_id}) from ${source_commit}"

sparkle_root="$(MACOS_APP_CONFIG="${CONFIG}" "${SCRIPT_DIR}/fetch-sparkle.sh")"
[[ -d "${sparkle_root}/${framework_rel}" ]] || die "verified Sparkle framework missing from ${sparkle_root}"

rm -rf "${APP_PATH}"
mkdir -p \
  "${APP_PATH}/Contents/MacOS" \
  "${APP_PATH}/Contents/Helpers" \
  "${APP_PATH}/Contents/Frameworks" \
  "${APP_PATH}/Contents/Resources" \
  "${BUILD_ROOT}/swift" \
  "${BUILD_ROOT}/helper" \
  "${BUILD_ROOT}/gomodcache" \
  "${BUILD_ROOT}/gocache"

swift_binaries=()
swift_bin_dirs=()
swift_build_flags=(-Xswiftc -DVEKIL_DEVELOPMENT_BUILD)
if [[ "${RELEASE_MODE}" == 1 ]]; then
  swift_build_flags=(-Xswiftc -DVEKIL_PRODUCTION_RELEASE)
fi
for arch in arm64 x86_64; do
  triple="${arch}-apple-macosx${minimum_system_version}"
  scratch="${BUILD_ROOT}/swift/${arch}"
  log "Building Swift product ${swift_product} for ${triple}"
  MACOSX_DEPLOYMENT_TARGET="${minimum_system_version}" \
    swift build \
      --package-path "${REPO_ROOT}/${swift_package_rel}" \
      --scratch-path "${scratch}" \
      --configuration release \
      --triple "${triple}" \
      --only-use-versions-from-resolved-file \
      "${swift_build_flags[@]}" \
      --product "${swift_product}" \
      -Xlinker -rpath \
      -Xlinker '@executable_path/../Frameworks'
  bin_dir="$(MACOSX_DEPLOYMENT_TARGET="${minimum_system_version}" swift build \
      --package-path "${REPO_ROOT}/${swift_package_rel}" \
      --scratch-path "${scratch}" \
      --configuration release \
      --triple "${triple}" \
      --only-use-versions-from-resolved-file \
      "${swift_build_flags[@]}" \
      --show-bin-path)"
  binary="${bin_dir}/${swift_product}"
  [[ -x "${binary}" ]] || die "Swift executable not found: ${binary}"
  swift_bin_dirs+=("${bin_dir}")
  swift_binaries+=("${binary}")
done

log "Creating universal Swift executable"
lipo -create "${swift_binaries[@]}" -output "${APP_PATH}/Contents/MacOS/${app_executable}"
chmod 0755 "${APP_PATH}/Contents/MacOS/${app_executable}"

helper_binaries=()
for arch in arm64 x86_64; do
  goarch="${arch}"
  [[ "${arch}" != x86_64 ]] || goarch=amd64
  output="${BUILD_ROOT}/helper/${helper_executable}-${arch}"
  log "Building Go helper for darwin/${goarch} with macOS ${minimum_system_version} load metadata"
  GOMODCACHE="${MACOS_GO_MOD_CACHE:-${BUILD_ROOT}/gomodcache}" \
  GOCACHE="${MACOS_GO_BUILD_CACHE:-${BUILD_ROOT}/gocache}" \
  GOFLAGS=-mod=readonly \
  CGO_ENABLED=1 \
  GOOS=darwin \
  GOARCH="${goarch}" \
  CC=clang \
  MACOSX_DEPLOYMENT_TARGET="${minimum_system_version}" \
    go build \
      -trimpath \
      -ldflags="-s -w -linkmode=external -extldflags=-mmacosx-version-min=${minimum_system_version} -X main.bundleBuildID=${bundle_build_id} -X main.buildVersion=${marketing_version}" \
      -o "${output}" \
      "${go_helper_package}"
  helper_binaries+=("${output}")
done

log "Creating universal Go helper"
lipo -create "${helper_binaries[@]}" -output "${APP_PATH}/Contents/Helpers/${helper_executable}"
chmod 0755 "${APP_PATH}/Contents/Helpers/${helper_executable}"

log "Copying verified Sparkle framework"
ditto "${sparkle_root}/${framework_rel}" "${APP_PATH}/Contents/Frameworks/${framework_rel}"

log "Copying app resources"
ditto "${REPO_ROOT}/${icon_rel}" "${APP_PATH}/Contents/Resources/Vekil.icns"
ditto "${RESOLVED_MANIFEST}" "${APP_PATH}/Contents/Resources/${release_manifest_name}"

source_resources="${REPO_ROOT}/${swift_package_rel}/Resources"
if [[ -d "${source_resources}" ]]; then
  while IFS= read -r -d '' resource; do
    [[ "$(basename "${resource}")" == "Info.plist" ]] && continue
    ditto "${resource}" "${APP_PATH}/Contents/Resources/$(basename "${resource}")"
  done < <(find "${source_resources}" -mindepth 1 -maxdepth 1 -print0)
fi

# SwiftPM emits target resource bundles next to the product. The resources must
# be architecture-independent; compare both builds before copying them.
arm_bin_dir="${swift_bin_dirs[0]}"
x86_bin_dir="${swift_bin_dirs[1]}"
while IFS= read -r -d '' bundle; do
  bundle_name="$(basename "${bundle}")"
  peer="${x86_bin_dir}/${bundle_name}"
  [[ -d "${peer}" ]] || die "resource bundle missing from x86_64 build: ${bundle_name}"
  diff -qr "${bundle}" "${peer}" >/dev/null || die "resource bundle differs by architecture: ${bundle_name}"
  ditto "${bundle}" "${APP_PATH}/Contents/Resources/${bundle_name}"
done < <(find "${arm_bin_dir}" -mindepth 1 -maxdepth 1 -type d -name '*.bundle' -print0)

"${MANIFEST_TOOL}" plist \
  --manifest "${RESOLVED_MANIFEST}" \
  --output "${APP_PATH}/Contents/Info.plist"
printf 'APPL????' >"${APP_PATH}/Contents/PkgInfo"

APP_PATH="${APP_PATH}" MACOS_APP_CONFIG="${CONFIG}" MACOS_RELEASE="${RELEASE_MODE}" \
  "${SCRIPT_DIR}/sign-macos-app.sh"
APP_PATH="${APP_PATH}" MACOS_RESOLVED_MANIFEST="${RESOLVED_MANIFEST}" MACOS_RELEASE="${RELEASE_MODE}" \
  "${SCRIPT_DIR}/verify-macos-app.sh"

log "Assembled ${APP_PATH}"
