#!/usr/bin/env bash

set -euo pipefail

[[ "$(uname -s)" == Darwin ]] || {
  echo "error: generated Sparkle appcast test requires macOS" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vekil-appcast-generation-test.XXXXXX")"
trap 'rm -rf "${TMP_ROOT}"' EXIT

for command in openssl ditto base64 tail shasum; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "error: missing required command: ${command}" >&2
    exit 1
  }
done

manifest="${TMP_ROOT}/vekil-macos-release.json"
app="${TMP_ROOT}/Vekil.app"
release_dir="${TMP_ROOT}/release"
mkdir -p "${app}/Contents/MacOS" "${release_dir}"

"${REPO_ROOT}/scripts/macos-release-manifest.py" resolve \
  --output "${manifest}" \
  --marketing-version 0.15.0-test.1 \
  --bundle-version 15001 \
  --bundle-build-id appcast-generation-test \
  --previous-bundle-version 14000 \
  --source-commit 06a7fa15af95d611b73280be8823089073fbd9f0 \
  --release

openssl genpkey -algorithm Ed25519 -out "${TMP_ROOT}/test-key.pem" >/dev/null 2>&1
openssl pkey -in "${TMP_ROOT}/test-key.pem" -outform DER -out "${TMP_ROOT}/test-private.der"
openssl pkey -in "${TMP_ROOT}/test-key.pem" -pubout -outform DER -out "${TMP_ROOT}/test-public.der"
private_seed="$(tail -c 32 "${TMP_ROOT}/test-private.der" | base64 | tr -d '\r\n')"
public_key="$(tail -c 32 "${TMP_ROOT}/test-public.der" | base64 | tr -d '\r\n')"
legacy_artifact="${TMP_ROOT}/vekil-macos-arm64.zip"
printf 'legacy-zip-fixture' >"${legacy_artifact}"
legacy_artifact_size="$(wc -c <"${legacy_artifact}" | tr -d ' ')"
legacy_artifact_sha256="$(shasum -a 256 "${legacy_artifact}" | awk '{print $1}')"
legacy_artifact_url="https://example.invalid/v0.14.1/vekil-macos-arm64.zip"
openssl pkeyutl -sign \
  -inkey "${TMP_ROOT}/test-key.pem" \
  -rawin \
  -in "${legacy_artifact}" \
  -out "${TMP_ROOT}/legacy-artifact-signature"
legacy_signature="$(base64 <"${TMP_ROOT}/legacy-artifact-signature" | tr -d '\r\n')"
python3 - \
  "${manifest}" \
  "${public_key}" \
  "${legacy_artifact_url}" \
  "${legacy_artifact_sha256}" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
value = json.loads(path.read_text())
value["sparkle"]["public_ed_key"] = sys.argv[2]
value["legacy_shell"]["last_compatible_artifact_url"] = sys.argv[3]
value["legacy_shell"]["last_compatible_artifact_sha256"] = sys.argv[4]
path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
PY

"${REPO_ROOT}/scripts/macos-release-manifest.py" plist \
  --manifest "${manifest}" \
  --output "${app}/Contents/Info.plist"
printf '#!/bin/sh\nexit 0\n' >"${app}/Contents/MacOS/Vekil"
chmod 0755 "${app}/Contents/MacOS/Vekil"
ditto -c -k --sequesterRsrc --keepParent "${app}" "${release_dir}/vekil-macos-universal.zip"
before_sha256="$(shasum -a 256 "${release_dir}/vekil-macos-universal.zip" | awk '{print $1}')"

signature="$(python3 - <<'PY'
import base64
print(base64.b64encode(bytes(range(64))).decode())
PY
)"
cat >"${TMP_ROOT}/base-appcast.xml" <<EOF_APPCAST
<?xml version="1.0"?>
<rss xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle" version="2.0">
  <channel>
    <item>
      <title>0.14.1</title>
      <sparkle:version>0.14.1</sparkle:version>
      <sparkle:shortVersionString>0.14.1</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>10.13</sparkle:minimumSystemVersion>
      <sparkle:hardwareRequirements>arm64</sparkle:hardwareRequirements>
      <enclosure url="${legacy_artifact_url}" length="${legacy_artifact_size}" type="application/octet-stream" sparkle:edSignature="${legacy_signature}"/>
    </item>
  </channel>
</rss><!-- sparkle-signatures:
edSignature: ${signature}
length: 1
-->
EOF_APPCAST

SPARKLE_PRIVATE_ED_KEY="${private_seed}" \
SPARKLE_BASE_APPCAST_FILE="${TMP_ROOT}/base-appcast.xml" \
SPARKLE_LEGACY_ARTIFACT_FILE="${legacy_artifact}" \
SPARKLE_REQUIRE_BASE_APPCAST=1 \
SPARKLE_DOWNLOAD_URL_PREFIX=https://example.invalid/v0.15.0-test.1 \
SPARKLE_RELEASE_NOTES_URL=https://example.invalid/v0.15.0-test.1 \
  "${REPO_ROOT}/scripts/generate-sparkle-appcast.sh" \
    "${release_dir}/vekil-macos-universal.zip" \
    "${manifest}" \
    "${release_dir}/appcast.xml"

after_sha256="$(shasum -a 256 "${release_dir}/vekil-macos-universal.zip" | awk '{print $1}')"
[[ "${before_sha256}" == "${after_sha256}" ]] || {
  echo "error: generated appcast changed the candidate ZIP" >&2
  exit 1
}

"${REPO_ROOT}/scripts/verify-sparkle-appcast.py" \
  --appcast "${release_dir}/appcast.xml" \
  --manifest "${manifest}" \
  --artifact "${release_dir}/vekil-macos-universal.zip" \
  --legacy-artifact "${legacy_artifact}" \
  --expected-url-prefix https://example.invalid/v0.15.0-test.1 \
  --require-legacy-compatible-entry >/dev/null

printf 'generated Sparkle appcast macOS 10.13/12/13 eligibility test passed\n'
