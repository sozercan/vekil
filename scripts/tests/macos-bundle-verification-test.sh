#!/usr/bin/env bash

set -euo pipefail

[[ "$(uname -s)" == Darwin ]] || {
  echo "error: synthetic bundle verification test requires macOS" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-bundle-verification-test.XXXXXX")"
trap 'rm -rf "${TMP_ROOT}"' EXIT
APP_PATH="${TMP_ROOT}/Vekil.app"
MANIFEST="${TMP_ROOT}/vekil-macos-release.json"

for directory in \
  "${APP_PATH}/Contents/MacOS" \
  "${APP_PATH}/Contents/Helpers" \
  "${APP_PATH}/Contents/Frameworks" \
  "${APP_PATH}/Contents/Resources"; do
  mkdir -p "${directory}"
done

"${REPO_ROOT}/scripts/macos-release-manifest.py" resolve \
  --output "${MANIFEST}" \
  --marketing-version 0.0.0-test \
  --bundle-version 999001 \
  --bundle-build-id bundle-verification-test \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0
"${REPO_ROOT}/scripts/macos-release-manifest.py" plist \
  --manifest "${MANIFEST}" \
  --output "${APP_PATH}/Contents/Info.plist"

cat >"${TMP_ROOT}/app.m" <<'EOF_APP'
#import <Sparkle/Sparkle.h>
int main(void) { return [SPUStandardUpdaterController class] == Nil; }
EOF_APP
cat >"${TMP_ROOT}/helper.c" <<'EOF_HELPER'
int main(void) { return 0; }
EOF_HELPER

sparkle_root="$("${REPO_ROOT}/scripts/fetch-sparkle.sh")"
for arch in arm64 x86_64; do
  clang \
    -x objective-c -fobjc-arc \
    -arch "${arch}" \
    -mmacosx-version-min=13.0 \
    -F"${sparkle_root}" \
    -framework Sparkle \
    -framework Foundation \
    -Wl,-rpath,@executable_path/../Frameworks \
    "${TMP_ROOT}/app.m" \
    -o "${TMP_ROOT}/Vekil-${arch}"
  clang \
    -arch "${arch}" \
    -mmacosx-version-min=13.0 \
    "${TMP_ROOT}/helper.c" \
    -o "${TMP_ROOT}/vekil-runtime-${arch}"
done

lipo -create \
  "${TMP_ROOT}/Vekil-arm64" \
  "${TMP_ROOT}/Vekil-x86_64" \
  -output "${APP_PATH}/Contents/MacOS/Vekil"
lipo -create \
  "${TMP_ROOT}/vekil-runtime-arm64" \
  "${TMP_ROOT}/vekil-runtime-x86_64" \
  -output "${APP_PATH}/Contents/Helpers/vekil-runtime"
ditto "${sparkle_root}/Sparkle.framework" "${APP_PATH}/Contents/Frameworks/Sparkle.framework"
ditto "${REPO_ROOT}/assets/macos/Vekil.icns" "${APP_PATH}/Contents/Resources/Vekil.icns"
ditto "${MANIFEST}" "${APP_PATH}/Contents/Resources/vekil-macos-release.json"

APP_PATH="${APP_PATH}" "${REPO_ROOT}/scripts/sign-macos-app.sh"
APP_PATH="${APP_PATH}" \
MACOS_RESOLVED_MANIFEST="${MANIFEST}" \
  "${REPO_ROOT}/scripts/verify-macos-app.sh"

# Structural signature verification does not exercise dyld library validation.
# Execute both slices from the assembled bundle so ad-hoc development signing
# cannot silently leave Sparkle unloadable.
/usr/bin/arch -arm64 "${APP_PATH}/Contents/MacOS/Vekil"
/usr/bin/arch -x86_64 "${APP_PATH}/Contents/MacOS/Vekil"

printf 'synthetic universal macOS bundle/signature verification test passed\n'
